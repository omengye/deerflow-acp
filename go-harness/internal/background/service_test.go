package background

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type fixtureExecutor struct {
	run func(context.Context, *bt.Task, bt.ExecutionRuntime) (*bt.ExecutionResult, error)
}

func (*fixtureExecutor) Key() string                                       { return "harness.fixture" }
func (*fixtureExecutor) LeaseExpiryPolicy() bt.LeaseExpiryPolicy           { return bt.LeaseExpiryRetry }
func (*fixtureExecutor) ValidateSpec(bt.Spec) error                        { return nil }
func (*fixtureExecutor) ValidateExecution(context.Context, *bt.Task) error { return nil }
func (*fixtureExecutor) SupportsDrain() bool                               { return true }
func (f *fixtureExecutor) Execute(ctx context.Context, task *bt.Task, runtime bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
	if f.run != nil {
		return f.run(ctx, task, runtime)
	}
	return &bt.ExecutionResult{Status: bt.StatusCompleted, Data: []byte("done")}, nil
}

type fixtureAuthorizer struct{}

func (fixtureAuthorizer) AuthorizeTaskAccess(_ context.Context, actor harness.TaskActor) error {
	if actor.OwnerID != "owner-"+actor.SessionID {
		return harness.ErrPermissionDenied
	}
	return nil
}

type fixtureBudget struct{ failBind, failCommit atomic.Bool }

func (b *fixtureBudget) BindTaskTx(ctx context.Context, tx *sql.Tx, binding Binding) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO fixture_budget(task_id,commits) VALUES(?,0)", binding.TaskID); err != nil {
		return err
	}
	if b.failBind.Load() {
		return errors.New("fixture bind fault")
	}
	return nil
}
func (*fixtureBudget) BeforeAttempt(context.Context, TaskScope) error { return nil }
func (b *fixtureBudget) CommitAttemptTx(ctx context.Context, tx *sql.Tx, scope TaskScope, _ string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE fixture_budget SET commits=commits+1 WHERE task_id=?", scope.Binding.TaskID); err != nil {
		return err
	}
	if b.failCommit.Load() {
		return errors.New("fixture commit fault")
	}
	return nil
}

type fixtureFactory struct {
	open func(context.Context, TaskScope) (*Attempt, error)
}

func (*fixtureFactory) Validate(context.Context, Binding) error { return nil }
func (f *fixtureFactory) Open(ctx context.Context, scope TaskScope) (*Attempt, error) {
	return f.open(ctx, scope)
}

func openFixtureStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = s.DB().Exec("CREATE TABLE IF NOT EXISTS fixture_budget(task_id TEXT PRIMARY KEY, commits INTEGER)"); err != nil {
		t.Fatal(err)
	}
	return s
}
func newFixture(t *testing.T, config func(*Config)) (*Service, *fixtureBudget) {
	t.Helper()
	store := openFixtureStore(t, filepath.Join(t.TempDir(), "background.db"))
	budget := &fixtureBudget{}
	cfg := Config{Store: store, Authorizer: fixtureAuthorizer{}, Budgets: budget, AdditionalExecutors: []bt.Executor{&fixtureExecutor{}}, PollInterval: 5 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond, NotificationLease: 20 * time.Millisecond}
	if config != nil {
		config(&cfg)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.DrainAndClose(ctx)
	})
	return s, budget
}
func actor(session string) harness.TaskActor {
	return harness.TaskActor{OwnerID: "owner-" + session, SessionID: session}
}
func submission(session, origin string) Submission {
	return Submission{Binding: Binding{ParentSessionID: session, OriginRunID: "run-1", OriginToolCallID: origin, Workspace: "workspace-a", ExecutionContract: "contract-v1", RootBudgetID: "root-1", ConfigVersion: 1, AgentVersion: "fixture-v1"}, Spec: bt.Spec{ExecutorKey: "harness.fixture", Kind: "fixture", Payload: []byte(`{"input":"hello"}`), NotifySession: true}}
}
func submitFixture(t *testing.T, s *Service, in Submission) harness.BackgroundTask {
	t.Helper()
	result, err := s.Submit(context.Background(), actor(in.Binding.ParentSessionID), in)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func countRows(t *testing.T, store *sqlite.Store, table string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func waitCondition(t *testing.T, f func() bool) {
	t.Helper()
	end := time.After(3 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !f() {
		select {
		case <-end:
			t.Fatal("condition did not become true")
		case <-tick.C:
		}
	}
}

func TestSubmissionAtomicAndIdempotent(t *testing.T) {
	s, budget := newFixture(t, nil)
	ctx := context.Background()
	budget.failBind.Store(true)
	if _, err := s.Submit(ctx, actor("parent"), submission("parent", "origin")); err == nil {
		t.Fatal("wanted bind fault")
	}
	for _, table := range []string{"eino_background_tasks", "harness_background_bindings", "fixture_budget", "eino_task_notifications"} {
		if n := countRows(t, s.store, table); n != 0 {
			t.Fatalf("%s has %d rows after rollback", table, n)
		}
	}
	budget.failBind.Store(false)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, err := s.Submit(ctx, actor("parent"), submission("parent", "origin"))
			errs <- err
			ids <- task.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for got := range ids {
		if id != "" && got != id {
			t.Fatal("origin allocated multiple task IDs")
		}
		id = got
	}
	if countRows(t, s.store, "eino_background_tasks") != 1 || countRows(t, s.store, "fixture_budget") != 1 {
		t.Fatal("origin replay duplicated durable work")
	}
	changed := submission("parent", "origin")
	changed.Spec.Payload = []byte(`{"input":"changed"}`)
	if _, err := s.Submit(ctx, actor("parent"), changed); !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatalf("changed input: %v", err)
	}
	changed = submission("parent", "origin")
	changed.Binding.ConfigVersion++
	if _, err := s.Submit(ctx, actor("parent"), changed); !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatalf("changed policy: %v", err)
	}
}

func TestOwnershipAndChildSessionAuthority(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	task := submitFixture(t, s, submission("parent", "origin"))
	if _, err := s.Get(ctx, harness.TaskActor{OwnerID: "intruder", SessionID: "parent"}, task.ID); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("owner check: %v", err)
	}
	if _, err := s.Get(ctx, actor("other"), task.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("cross-session read: %v", err)
	}
	if _, err := s.Cancel(ctx, actor("other"), task.ID, "stop"); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("cross-session cancel: %v", err)
	}
	if got, err := s.List(ctx, actor("other"), "", 100); err != nil || len(got) != 0 {
		t.Fatalf("cross-session list: %v %v", got, err)
	}
	for _, child := range []string{"parent", "arbitrary-existing-session"} {
		in := submission("parent", child)
		in.Binding.ChildSessionID = child
		if _, err := s.Submit(ctx, actor("parent"), in); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("child %s: %v", child, err)
		}
	}
	in := submission("other", "reuse")
	in.Binding.ChildSessionID = task.ChildSessionID
	if _, err := s.Submit(ctx, actor("other"), in); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-parent continuation: %v", err)
	}
	in = submission("parent", "reuse")
	in.Binding.ChildSessionID = task.ChildSessionID
	second := submitFixture(t, s, in)
	if second.ChildSessionID != task.ChildSessionID {
		t.Fatal("continuation did not retain authorized child")
	}
}

func TestWorkersBoundedAndDiscoverPendingAfterReopen(t *testing.T) {
	var running, peak atomic.Int64
	release := make(chan struct{})
	executor := &fixtureExecutor{run: func(ctx context.Context, _ *bt.Task, rt bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		select {
		case <-release:
			return &bt.ExecutionResult{Status: bt.StatusCompleted}, nil
		case <-rt.Controls():
			return &bt.ExecutionResult{Status: bt.StatusSuspended, Checkpoint: []byte("pause")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	s, _ := newFixture(t, func(c *Config) { c.MaxWorkers = 2; c.AdditionalExecutors = []bt.Executor{executor} })
	var tasks []harness.BackgroundTask
	for i := 0; i < 7; i++ {
		tasks = append(tasks, submitFixture(t, s, submission("parent", fmt.Sprintf("origin-%d", i))))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := s.DrainAndClose(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	// A new client over the durable store discovers accepted pending work.
	reopened, err := New(context.Background(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = reopened.DrainAndClose(ctx)
	})
	if err = reopened.StartWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitCondition(t, func() bool { return running.Load() == 2 })
	// More pending jobs than workers+queue must not starve notification intake.
	waitCondition(t, func() bool { return countRows(t, s.store, "harness_background_inbox") >= len(tasks) })
	close(release)
	waitCondition(t, func() bool {
		for _, task := range tasks {
			got, err := reopened.Get(context.Background(), actor("parent"), task.ID)
			if err != nil || got.Status != "completed" {
				return false
			}
		}
		return true
	})
	if peak.Load() != 2 {
		t.Fatalf("peak workers=%d", peak.Load())
	}
}
