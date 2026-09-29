package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/cloudwego/eino/adk/backgroundtask/storetest"
	"github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/schema"
)

func testStore(t testing.TB) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestSessionConformance(t *testing.T) {
	session.RunConformanceTests(t, func(t testing.TB) adk.SessionEventStore[*schema.Message] { return testStore(t) }, func(s string) *schema.Message { return schema.UserMessage(s) })
}

func TestSerializerConformance(t *testing.T) {
	session.RunSerializerConformanceTests(t, func(t testing.TB, serializer schema.Serializer) adk.SessionEventStore[*schema.Message] {
		s, err := OpenWithConfig(filepath.Join(t.TempDir(), "custom.db"), Config{EventSerializer: serializer})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}, func(s string) *schema.Message { return schema.UserMessage(s) })
}

func TestTaskStoreConformance(t *testing.T) {
	var clocks sync.Map
	factory := func(t testing.TB) bt.TaskStore {
		s := testStore(t).Tasks()
		offset := new(atomic.Int64)
		s.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
		clocks.Store(s, offset)
		return s
	}
	expire := func(t testing.TB, s bt.TaskStore, _ *bt.Task) {
		v, _ := clocks.Load(s)
		v.(*atomic.Int64).Add(int64(time.Hour))
	}
	storetest.RunTaskStoreConformance(t, storetest.TaskStoreConfig{New: factory, ExpireActiveAttempt: expire})
}

func TestTaskEventConformance(t *testing.T) {
	storetest.RunTaskEventStoreConformance(t, storetest.TaskEventStoreConfig{New: func(t testing.TB) (bt.TaskStore, bt.TaskEventStore) { s := testStore(t).Tasks(); return s, s }})
}

func TestNotificationOutboxConformance(t *testing.T) {
	var clocks sync.Map
	storetest.RunNotificationOutboxConformance(t, storetest.NotificationOutboxConfig{
		New: func(t testing.TB) (bt.TaskStore, bt.NotificationOutbox) {
			s := testStore(t).Tasks()
			offset := new(atomic.Int64)
			base := time.Now()
			// The upstream conformance lease is only 20 ms. Advance it explicitly
			// so package scheduling cannot expire the receipt before Ack.
			s.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
			clocks.Store(s, offset)
			return s, s
		},
		ExpireLease: func(t testing.TB, s bt.NotificationOutbox, d time.Duration) {
			v, _ := clocks.Load(s)
			v.(*atomic.Int64).Add(int64(d + time.Second))
		},
	})
}

func TestNotificationWriterConformance(t *testing.T) {
	var clocks sync.Map
	storetest.RunNotificationWriterConformance(t, storetest.NotificationWriterConfig{
		New: func(t testing.TB) (bt.TaskStore, bt.NotificationOutbox) {
			s := testStore(t).Tasks()
			offset := new(atomic.Int64)
			s.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
			clocks.Store(s, offset)
			return s, s
		},
		ExpireActiveAttempt: func(t testing.TB, s bt.TaskStore, _ *bt.Task) {
			v, _ := clocks.Load(s)
			v.(*atomic.Int64).Add(int64(time.Hour))
		},
	})
}

func TestSessionBatchAtomicityAndIsolation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	event := func(id, content string) *adk.SessionEvent[*schema.Message] {
		return &adk.SessionEvent[*schema.Message]{EventID: id, Kind: adk.SessionEventMessage, Message: schema.UserMessage(content)}
	}
	if err := s.AppendEvents(ctx, "s", []*adk.SessionEvent[*schema.Message]{event("existing", "first")}); err != nil {
		t.Fatal(err)
	}
	err := s.AppendEvents(ctx, "s", []*adk.SessionEvent[*schema.Message]{event("new", "must rollback"), event("existing", "conflict")})
	if !errors.Is(err, adk.ErrDuplicateEventID) {
		t.Fatalf("expected duplicate, got %v", err)
	}
	out, err := s.LoadEvents(ctx, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("failed batch left %d events", len(out.Events))
	}
	out.Events[0].Message.Content = "changed"
	out, err = s.LoadEvents(ctx, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Events[0].Message.Content != "first" {
		t.Fatal("loaded data aliases persisted state")
	}
}

func TestSessionExactRetryAndRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	events := []*adk.SessionEvent[*schema.Message]{
		{EventID: "first", Kind: adk.SessionEventMessage, Message: schema.UserMessage("first")},
		{EventID: "idle-1", Kind: adk.SessionEventSessionStatusIdle, Lifecycle: &adk.LifecycleEvent{State: adk.SessionRunStateIdle, StopReason: &adk.StopReason{Type: adk.StopReasonEndTurn}}},
		{EventID: "second", Kind: adk.SessionEventMessage, Message: schema.UserMessage("second")},
		{EventID: "idle-2", Kind: adk.SessionEventSessionStatusIdle, Lifecycle: &adk.LifecycleEvent{State: adk.SessionRunStateIdle, StopReason: &adk.StopReason{Type: adk.StopReasonEndTurn}}},
	}
	if err := s.AppendEvents(ctx, "s", events); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents(ctx, "s", events); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if err := s.Set(ctx, "session/s/runner_checkpoint", []byte("stale")); err != nil {
		t.Fatal(err)
	}
	err := adk.RollbackSession(ctx, s, "s", "idle-1", adk.WithRollbackSessionCheckPointStore[*schema.Message](s), adk.WithRollbackSessionExpectedHeadEventID[*schema.Message]("wrong-head"))
	if !errors.Is(err, adk.ErrSessionHeadChanged) {
		t.Fatalf("expected stale head error: %v", err)
	}
	err = adk.RollbackSession(ctx, s, "s", "idle-1", adk.WithRollbackSessionCheckPointStore[*schema.Message](s), adk.WithRollbackSessionExpectedHeadEventID[*schema.Message]("idle-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get(ctx, "session/s/runner_checkpoint"); err != nil || ok {
		t.Fatalf("rollback retained checkpoint: %v %v", ok, err)
	}
	loaded, err := s.LoadEvents(ctx, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Events) != 5 || loaded.Events[4].Rollback == nil || loaded.Events[4].Rollback.ToEventID != "idle-1" {
		t.Fatalf("rollback log: %+v", loaded)
	}
}

func TestTaskCheckpointAndOutboxLeaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resume.db")
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: "job", ExecutorKey: "worker", SessionID: "session"}, LeaseExpiryPolicy: bt.LeaseExpiryRetry})
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: "job", ExpectedVersion: created.Version})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := s.Tasks().WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: "job", ExpectedVersion: started.Version, Checkpoint: []byte("tool state")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Tasks().Resume(ctx, &bt.ResumeRequest{TaskID: "job", ExpectedVersion: waiting.Version, Data: []byte("permission")})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.Tasks().Receive(ctx, &bt.ReceiveNotificationsRequest{LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Deliveries) != 1 {
		t.Fatalf("outbox entries=%d", len(batch.Deliveries))
	}
	receipt := append(bt.NotificationReceipt(nil), batch.Deliveries[0].Receipt...)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Tasks().Get(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Checkpoint) != "tool state" || string(got.PendingResume) != "permission" || got.Status != bt.StatusPending {
		t.Fatalf("reopened task: %+v", got)
	}
	batch, err = s.Tasks().Receive(ctx, &bt.ReceiveNotificationsRequest{LeaseDuration: time.Hour})
	if err != nil || len(batch.Deliveries) != 0 {
		t.Fatalf("reopen lost active lease: %+v %v", batch, err)
	}
	if err = s.Tasks().Ack(ctx, receipt); err != nil {
		t.Fatalf("persisted receipt rejected: %v", err)
	}
}

func TestCheckpointRoundTripAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, ok, err := s.Get(ctx, "missing"); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
	data := []byte("checkpoint")
	if err := s.Set(ctx, "cp", data); err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	got, ok, err := s.Get(ctx, "cp")
	if err != nil || !ok || string(got) != "checkpoint" {
		t.Fatalf("checkpoint: %q %v %v", got, ok, err)
	}
	got[0] = 'Y'
	got, _, err = s.Get(ctx, "cp")
	if err != nil || string(got) != "checkpoint" {
		t.Fatalf("alias: %q %v", got, err)
	}
	if err = s.Delete(ctx, "cp"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = s.Get(ctx, "cp"); err != nil || ok {
		t.Fatalf("deleted: %v %v", ok, err)
	}
	if err = s.Set(ctx, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok, err = s.Get(ctx, "empty"); err != nil || !ok || len(got) != 0 {
		t.Fatalf("empty: %q %v %v", got, ok, err)
	}
}

func TestConcurrentTaskClaimAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	created, err := a.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: "work", ExecutorKey: "worker"}, LeaseExpiryPolicy: bt.LeaseExpiryRetry})
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := a.Tasks()
			if i%2 == 1 {
				store = b.Tasks()
			}
			_, err := store.Start(ctx, &bt.StartTaskRequest{TaskID: created.Spec.ID, ExpectedVersion: created.Version})
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, bt.ErrVersionConflict) {
				t.Errorf("claim: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("successful claims=%d want 1", wins.Load())
	}
	got, err := b.Tasks().Get(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 || got.Version != 2 {
		t.Fatalf("invalid claimed task: %+v", got)
	}
}

func TestCrashDurability(t *testing.T) {
	const key = "DEERFLOW_SQLITE_CRASH_HELPER"
	if path := os.Getenv(key); path != "" {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err = s.Set(ctx, "cp", []byte("durable")); err != nil {
			t.Fatal(err)
		}
		if err = s.AppendEvents(ctx, "session", []*adk.SessionEvent[*schema.Message]{{EventID: "input", Kind: adk.SessionEventMessage, Message: schema.UserMessage("survives crash")}}); err != nil {
			t.Fatal(err)
		}
		created, err := s.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: "job", ExecutorKey: "worker", SessionID: "session"}, LeaseExpiryPolicy: bt.LeaseExpiryRetry})
		if err != nil {
			t.Fatal(err)
		}
		started, err := s.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: "job", ExpectedVersion: created.Version})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Tasks().AppendTaskEvent(ctx, &bt.AppendTaskEventRequest{TaskID: "job", Attempt: started.Attempt, EventID: "progress", Data: []byte("persisted")}); err != nil {
			t.Fatal(err)
		}
		// Abrupt exit deliberately skips Close/checkpointing the WAL.
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCrashDurability$")
	cmd.Env = append(os.Environ(), key+"="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, out)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	got, ok, err := s.Get(ctx, "cp")
	if err != nil || !ok || string(got) != "durable" {
		t.Fatalf("checkpoint lost: %q %v %v", got, ok, err)
	}
	events, err := s.LoadEvents(ctx, "session", nil)
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("session lost: %+v %v", events, err)
	}
	task, err := s.Tasks().Get(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != bt.StatusRunning || task.Attempt != 1 {
		t.Fatalf("task lost: %+v", task)
	}
	progress, err := s.Tasks().ListTaskEvents(ctx, &bt.ListTaskEventsRequest{TaskID: "job"})
	if err != nil || len(progress.Events) != 1 {
		t.Fatalf("progress lost: %+v %v", progress, err)
	}
	batch, err := s.Tasks().Receive(ctx, &bt.ReceiveNotificationsRequest{LeaseDuration: time.Second})
	if err != nil || len(batch.Deliveries) != 1 {
		t.Fatalf("outbox lost: %+v %v", batch, err)
	}
}

func TestTaskWaitObservesAnotherConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wait.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	task, err := a.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: "job", ExecutorKey: "worker"}, LeaseExpiryPolicy: bt.LeaseExpiryRetry})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		got, err := a.Tasks().WaitForTaskVersion(ctx, &bt.WaitForTaskVersionRequest{TaskID: "job", AfterVersion: task.Version})
		if err == nil && got.Status != bt.StatusRunning {
			err = fmt.Errorf("wait status: %s", got.Status)
		}
		done <- err
	}()
	if _, err = b.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: "job", ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOutboxFailureRollsBackStateAndReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	installFailure := func() {
		t.Helper()
		_, err := s.DB().Exec(`CREATE TRIGGER reject_test_outbox BEFORE INSERT ON eino_task_notifications BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END`)
		if err != nil {
			t.Fatal(err)
		}
	}
	removeFailure := func() {
		t.Helper()
		if _, err := s.DB().Exec("DROP TRIGGER reject_test_outbox"); err != nil {
			t.Fatal(err)
		}
	}
	req := &bt.CreateTaskRequest{Spec: bt.Spec{ID: "job", ExecutorKey: "worker", SessionID: "session", NotifySession: true}, LeaseExpiryPolicy: bt.LeaseExpiryRetry}
	installFailure()
	if _, err := s.Tasks().Create(ctx, req); err == nil {
		t.Fatal("expected create outbox failure")
	}
	if _, err := s.Tasks().Get(ctx, "job"); !errors.Is(err, bt.ErrNotFound) {
		t.Fatalf("failed create left task: %v", err)
	}
	removeFailure()
	created, err := s.Tasks().Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: "job", ExpectedVersion: created.Version})
	if err != nil {
		t.Fatal(err)
	}
	notification := &bt.NotifyParentRequest{EventID: "progress-1", Kind: "progress", Data: []byte("hello")}
	installFailure()
	if err = s.Tasks().EnqueueTaskNotification(ctx, "job", started.Attempt, notification); err == nil {
		t.Fatal("expected custom notification outbox failure")
	}
	var count int
	if err = s.DB().QueryRow("SELECT COUNT(*) FROM eino_notification_replays WHERE task_id=?", "job").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed notification retained replay metadata")
	}
	if _, err = s.Tasks().Complete(ctx, &bt.CompleteTaskRequest{TaskID: "job", ExpectedVersion: started.Version, Data: []byte("done")}); err == nil {
		t.Fatal("expected terminal outbox failure")
	}
	got, err := s.Tasks().Get(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != started.Version || got.Status != bt.StatusRunning {
		t.Fatalf("failed completion persisted: %+v", got)
	}
	removeFailure()
	if err = s.Tasks().EnqueueTaskNotification(ctx, "job", started.Attempt, notification); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if _, err = s.Tasks().Complete(ctx, &bt.CompleteTaskRequest{TaskID: "job", ExpectedVersion: started.Version, Data: []byte("done")}); err != nil {
		t.Fatal(err)
	}
}
