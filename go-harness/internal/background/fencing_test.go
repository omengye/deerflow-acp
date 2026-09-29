package background

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func claimFixture(t *testing.T, s *Service, id string) (*bt.Task, context.Context, *attemptState) {
	t.Helper()
	ctx := context.Background()
	task, err := s.tasks.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.tasks.Start(ctx, &bt.StartTaskRequest{TaskID: id, ExpectedVersion: task.Version})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := loadBinding(ctx, s.store.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	state := &attemptState{scope: TaskScope{Binding: b, Attempt: task.Attempt}, writes: map[string]checkpointWrite{}}
	s.mu.Lock()
	s.attempts[attemptKey(state.scope)] = state
	s.mu.Unlock()
	return task, context.WithValue(ctx, attemptContextKey{}, &attemptContext{state: state}), state
}

func fixtureEvent(id string) []*adk.SessionEvent[*schema.Message] {
	return []*adk.SessionEvent[*schema.Message]{{EventID: id, Kind: adk.SessionEventMessage, Message: schema.UserMessage("hello")}}
}

func TestChildLeaseClaimRollsBackAndFencesExpiredAttempt(t *testing.T) {
	s, _ := newFixture(t, nil)
	first := submitFixture(t, s, submission("parent", "one"))
	in := submission("parent", "two")
	in.Binding.ChildSessionID = first.ChildSessionID
	second := submitFixture(t, s, in)
	task, oldContext, _ := claimFixture(t, s, first.ID)
	if _, err := s.tasks.Start(context.Background(), &bt.StartTaskRequest{TaskID: second.ID, ExpectedVersion: second.Version}); !errors.Is(err, harness.ErrChildSessionBusy) {
		t.Fatalf("second child claim: %v", err)
	}
	unchanged, err := s.tasks.Get(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Attempt != 0 || unchanged.Version != second.Version || unchanged.Status != bt.StatusPending {
		t.Fatalf("rejected claim mutated task: %+v", unchanged)
	}
	oldStore, err := s.SessionStoreForAttempt(oldContext)
	if err != nil {
		t.Fatal(err)
	}
	if err = oldStore.AppendEvents(oldContext, first.ChildSessionID, fixtureEvent("before-expiry")); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckpointsForAttempt().Set(oldContext, first.ID+"/checkpoint", []byte("uncommitted")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.DB().Exec("UPDATE eino_background_tasks SET lease_expires_at=1 WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	// A replacement client has no old process-local admission state.
	replacement, err := New(context.Background(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := replacement.tasks.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status != bt.StatusPending {
		t.Fatalf("expiry: %s", expired.Status)
	}
	current, _, _ := claimFixture(t, replacement, first.ID)
	if current.Attempt != task.Attempt+1 {
		t.Fatal("replacement did not acquire a new fence")
	}
	if err = oldStore.AppendEvents(oldContext, first.ChildSessionID, fixtureEvent("stale")); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stale append: %v", err)
	}
	if err = s.CheckpointsForAttempt().Set(oldContext, first.ID+"/checkpoint", []byte("stale")); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stale checkpoint: %v", err)
	}
	if err = s.CheckEffect(oldContext); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stale effect: %v", err)
	}
	if countRows(t, s.store, "eino_session_events") != 1 {
		t.Fatal("stale attempt wrote a session event")
	}
	if _, ok, err := s.store.Get(context.Background(), first.ID+"/checkpoint"); err != nil || ok {
		t.Fatalf("staged checkpoint leaked to persistence: %v %v", ok, err)
	}
}

func TestCheckpointCommitIsAtomicAndRequiresCleanup(t *testing.T) {
	s, budget := newFixture(t, nil)
	submitted := submitFixture(t, s, submission("parent", "one"))
	task, ctx, state := claimFixture(t, s, submitted.ID)
	key := task.Spec.ID + "/checkpoint"
	if err := s.store.Set(ctx, key, []byte("old-safe")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointsForAttempt().Set(ctx, key, []byte("new-safe")); err != nil {
		t.Fatal(err)
	}
	pause := &bt.WaitInputTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Checkpoint: []byte("new-native-metadata")}
	if _, err := s.tasks.WaitInput(ctx, pause); err == nil {
		t.Fatal("published pause before cleanup")
	}
	data, _, err := s.store.Get(ctx, key)
	if err != nil || string(data) != "old-safe" {
		t.Fatalf("checkpoint before join: %s %v", data, err)
	}
	state.mu.Lock()
	state.ready = true
	state.joined = true
	state.mu.Unlock()
	// Budget rejection rolls back both its own mutation and staged raw bytes.
	budget.failCommit.Store(true)
	if _, err = s.tasks.WaitInput(ctx, pause); err == nil {
		t.Fatal("wanted budget failure")
	}
	budget.failCommit.Store(false)
	// Fail after the hook's checkpoint write to prove the shared transaction.
	if _, err = s.store.DB().Exec("CREATE TRIGGER fixture_pause_fault BEFORE UPDATE ON eino_background_tasks WHEN NEW.status='waiting_input' BEGIN SELECT RAISE(ABORT,'fixture task write fault'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.tasks.WaitInput(ctx, pause); err == nil {
		t.Fatal("wanted task persistence failure")
	}
	data, _, err = s.store.Get(ctx, key)
	if err != nil || string(data) != "old-safe" {
		t.Fatalf("checkpoint escaped rollback: %s %v", data, err)
	}
	var commits int
	if err = s.store.DB().QueryRow("SELECT commits FROM fixture_budget WHERE task_id=?", task.Spec.ID).Scan(&commits); err != nil || commits != 0 {
		t.Fatalf("budget escaped rollback: %d %v", commits, err)
	}
	if countRows(t, s.store, "eino_task_notifications") != 1 {
		t.Fatal("pause notification escaped rollback")
	}
	if _, err = s.store.DB().Exec("DROP TRIGGER fixture_pause_fault"); err != nil {
		t.Fatal(err)
	}
	paused, err := s.tasks.WaitInput(ctx, pause)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != bt.StatusWaitingInput || string(paused.Checkpoint) != "new-native-metadata" {
		t.Fatalf("bad pause: %+v", paused)
	}
	data, _, err = s.store.Get(ctx, key)
	if err != nil || string(data) != "new-safe" {
		t.Fatalf("raw checkpoint missing: %s %v", data, err)
	}
	if countRows(t, s.store, "eino_task_notifications") != 2 {
		t.Fatal("pause did not atomically emit notification")
	}
	if countRows(t, s.store, "harness_background_child_leases") != 0 {
		t.Fatal("paused child lease retained")
	}
	if err = s.CheckpointsForAttempt().Set(ctx, key, []byte("late")); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("late checkpoint: %v", err)
	}
}

func TestLateWritesAndFailedAttemptPreserveSafeCheckpoint(t *testing.T) {
	s, _ := newFixture(t, nil)
	submitted := submitFixture(t, s, submission("parent", "one"))
	task, ctx, state := claimFixture(t, s, submitted.ID)
	key := task.Spec.ID + "/checkpoint"
	if err := s.store.Set(ctx, key, []byte("safe")); err != nil {
		t.Fatal(err)
	}
	if err := (checkpointDispatcher{service: s}).Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	child, err := s.SessionStoreForAttempt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.joined = true
	state.ready = true
	state.failed = true
	state.mu.Unlock()
	if err = child.AppendEvents(ctx, submitted.ChildSessionID, fixtureEvent("late")); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("append after join: %v", err)
	}
	if err = s.CheckEffect(ctx); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("effect after join: %v", err)
	}
	if _, err = s.tasks.Fail(ctx, &bt.FailTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Error: "fixture failed"}); err != nil {
		t.Fatal(err)
	}
	data, ok, err := s.store.Get(ctx, key)
	if err != nil || !ok || string(data) != "safe" {
		t.Fatalf("failed attempt consumed checkpoint: %s %v %v", data, ok, err)
	}
}

func TestInboxCommitBeforeAckAndReplay(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	submitFixture(t, s, submission("parent", "one"))
	if _, err := s.store.DB().Exec("CREATE TRIGGER fixture_inbox_fault BEFORE INSERT ON harness_background_inbox BEGIN SELECT RAISE(ABORT,'inbox fault'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverNotifications(ctx); err == nil {
		t.Fatal("wanted inbox failure")
	}
	if countRows(t, s.store, "eino_task_notifications") != 1 || countRows(t, s.store, "harness_background_inbox") != 0 {
		t.Fatal("acked before committing inbox")
	}
	for _, sql := range []string{"DROP TRIGGER fixture_inbox_fault", "UPDATE eino_task_notifications SET lease_expires_at=1", "CREATE TRIGGER fixture_ack_fault BEFORE DELETE ON eino_task_notifications BEGIN SELECT RAISE(ABORT,'ack fault'); END"} {
		if _, err := s.store.DB().Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeliverNotifications(ctx); err == nil {
		t.Fatal("wanted ack failure")
	}
	if countRows(t, s.store, "harness_background_inbox") != 1 || countRows(t, s.store, "eino_task_notifications") != 1 {
		t.Fatal("commit-before-ack failed")
	}
	for _, sql := range []string{"DROP TRIGGER fixture_ack_fault", "UPDATE eino_task_notifications SET lease_expires_at=1"} {
		if _, err := s.store.DB().Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s.store, "harness_background_inbox") != 1 || countRows(t, s.store, "eino_task_notifications") != 0 {
		t.Fatal("outbox replay duplicated inbox")
	}
	items, err := s.ListInbox(ctx, actor("parent"), 0, 100)
	if err != nil || len(items) != 1 {
		t.Fatalf("inbox: %v %v", items, err)
	}
	if other, err := s.ListInbox(ctx, actor("other"), 0, 100); err != nil || len(other) != 0 {
		t.Fatalf("other inbox: %v %v", other, err)
	}
	if err = s.AcknowledgeInbox(ctx, actor("other"), items[0].ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("cross-session ack: %v", err)
	}
	if err = s.AcknowledgeInbox(ctx, actor("parent"), items[0].ID); err != nil {
		t.Fatal(err)
	}
	if items, err = s.ListInbox(ctx, actor("parent"), 0, 100); err != nil || len(items) != 0 {
		t.Fatalf("handled inbox: %v %v", items, err)
	}
}
