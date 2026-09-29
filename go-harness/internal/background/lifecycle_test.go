package background

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDrainAndCancelWaitForActualCleanup(t *testing.T) {
	for _, stop := range []string{"drain", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			started, joining, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var released atomic.Bool
			t.Cleanup(func() {
				if released.CompareAndSwap(false, true) {
					close(release)
				}
			})
			executor := &fixtureExecutor{run: func(ctx context.Context, _ *bt.Task, rt bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
				close(started)
				select {
				case control := <-rt.Controls():
					if control.Kind == bt.ControlStop {
						return &bt.ExecutionResult{Status: bt.StatusCanceled, Error: "stopped"}, nil
					}
					return &bt.ExecutionResult{Status: bt.StatusSuspended, Checkpoint: []byte("safe")}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			factory := &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
				return &Attempt{JoinAndClose: func(context.Context) error { close(joining); <-release; return nil }}, nil
			}}
			s, _ := newFixture(t, func(c *Config) {
				c.AdditionalExecutors = []bt.Executor{executor}
				c.Attempts = factory
				c.MaxWorkers = 1
			})
			task := submitFixture(t, s, submission("parent", "one"))
			if err := s.StartWorkers(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("not started")
			}
			if stop == "cancel" {
				if _, err := s.Cancel(context.Background(), actor("parent"), task.ID, "stop"); err != nil {
					t.Fatal(err)
				}
			}
			deadline, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			err := s.DrainAndClose(deadline)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("close returned without join: %v", err)
			}
			select {
			case <-joining:
			default:
				t.Fatal("cleanup was not invoked")
			}
			before, err := s.Get(context.Background(), actor("parent"), task.ID)
			if err != nil || before.Status != "running" {
				t.Fatalf("published outcome before join: %+v %v", before, err)
			}
			if _, err = s.Submit(context.Background(), actor("parent"), submission("parent", "new")); !errors.Is(err, harness.ErrBackgroundClosed) {
				t.Fatalf("submission after timeout: %v", err)
			}
			if released.CompareAndSwap(false, true) {
				close(release)
			}
			deadline, cancel = context.WithTimeout(context.Background(), time.Second)
			if err = s.DrainAndClose(deadline); err != nil {
				t.Fatal(err)
			}
			cancel()
			after, err := s.Get(context.Background(), actor("parent"), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "suspended"
			if stop == "cancel" {
				want = "canceled"
			}
			if after.Status != want {
				t.Fatalf("status=%s want %s", after.Status, want)
			}
			if err = s.store.DB().Ping(); err != nil {
				t.Fatalf("service closed shared DB: %v", err)
			}
			// Suspended tasks remain suspended on startup until explicitly released.
			if stop == "drain" {
				reopened, err := New(context.Background(), s.config)
				if err != nil {
					t.Fatal(err)
				}
				if err = reopened.StartWorkers(context.Background()); err != nil {
					t.Fatal(err)
				}
				waitCondition(t, func() bool {
					items, e := reopened.ListInbox(context.Background(), actor("parent"), 0, 10)
					return e == nil && len(items) > 0
				})
				got, err := reopened.Get(context.Background(), actor("parent"), task.ID)
				if err != nil || got.Status != "suspended" || got.Attempt != 1 {
					t.Fatalf("startup resumed suspended task: %+v %v", got, err)
				}
				deadline, cancel = context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err = reopened.DrainAndClose(deadline); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCleanupFailureQuarantinesAcrossRestartAndChildContinuation(t *testing.T) {
	factory := &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
		return &Attempt{JoinAndClose: func(context.Context) error { return errors.New("fixture unknown process cleanup") }}, nil
	}}
	s, _ := newFixture(t, func(c *Config) { c.Attempts = factory })
	task := submitFixture(t, s, submission("parent", "one"))
	if err := s.manager.Execute(context.Background(), task.ID); !errors.Is(err, harness.ErrBackgroundUncertain) {
		t.Fatalf("cleanup failure: %v", err)
	}
	got, err := s.Get(context.Background(), actor("parent"), task.ID)
	if err != nil || got.Status != "running" || got.BlockedReason == "" {
		t.Fatalf("missing quarantine: %+v %v", got, err)
	}
	if _, err = s.store.DB().Exec("UPDATE eino_background_tasks SET lease_expires_at=1 WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Get(context.Background(), actor("parent"), task.ID); err != nil {
		t.Fatal(err)
	}
	if err = reopened.manager.Execute(context.Background(), task.ID); !errors.Is(err, harness.ErrBackgroundUncertain) {
		t.Fatalf("restarted quarantined task: %v", err)
	}
	in := submission("parent", "two")
	in.Binding.ChildSessionID = task.ChildSessionID
	second := submitFixture(t, reopened, in)
	if err = reopened.manager.Execute(context.Background(), second.ID); !errors.Is(err, harness.ErrBackgroundUncertain) {
		t.Fatalf("uncertain child reused: %v", err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.DrainAndClose(deadline); !errors.Is(err, harness.ErrBackgroundUncertain) {
		t.Fatalf("close lost cleanup uncertainty: %v", err)
	}
}

type fixtureNativeAgent struct {
	name    string
	target  *atomic.Value
	resumed *atomic.Int64
}

func (a *fixtureNativeAgent) Name(context.Context) string      { return a.name }
func (*fixtureNativeAgent) Description(context.Context) string { return "fixture interrupt" }
func (a *fixtureNativeAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	event := adk.Interrupt(ctx, "approve")
	if event.Action != nil && event.Action.Interrupted != nil && len(event.Action.Interrupted.InterruptContexts) > 0 {
		a.target.Store(event.Action.Interrupted.InterruptContexts[0].ID)
	}
	gen.Send(event)
	gen.Close()
	return iter
}
func (a *fixtureNativeAgent) Resume(ctx context.Context, _ *adk.ResumeInfo, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	a.resumed.Add(1)
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(adk.EventFromMessage(schema.AssistantMessage("approved", nil), nil, schema.Assistant, a.name))
	gen.Close()
	return iter
}

type fixtureBroker struct{ target *atomic.Value }

func (b *fixtureBroker) PrepareResume(_ context.Context, _ harness.TaskActor, _ Binding, request harness.TaskApproval) (ResumeGrant, error) {
	if request.ID != "approval" {
		return ResumeGrant{}, harness.ErrPermissionDenied
	}
	data, err := json.Marshal(map[string]any{b.target.Load().(string): true})
	return ResumeGrant{Data: data, Approval: request}, err
}
func (*fixtureBroker) CommitResumeTx(ctx context.Context, tx *sql.Tx, _ harness.TaskActor, binding Binding, _ ResumeGrant) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO fixture_approvals(task_id) VALUES(?)", binding.TaskID)
	return err
}

func TestNativeSubagentCheckpointAndApprovalResumeAfterReconstruction(t *testing.T) {
	var target atomic.Value
	var opened, joined, resumed atomic.Int64
	factory := &fixtureFactory{open: func(_ context.Context, scope TaskScope) (*Attempt, error) {
		opened.Add(1)
		return &Attempt{Agent: &fixtureNativeAgent{name: scope.Binding.AgentVersion, target: &target, resumed: &resumed}, JoinAndClose: func(context.Context) error { joined.Add(1); return nil }}, nil
	}}
	s, _ := newFixture(t, func(c *Config) {
		c.AdditionalExecutors = nil
		c.AgentNames = []string{"fixture-v1"}
		c.Attempts = factory
		c.Approvals = &fixtureBroker{target: &target}
	})
	if _, err := s.store.DB().Exec("CREATE TABLE fixture_approvals(task_id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	binding := submission("parent", "one").Binding
	input := &adk.AgentInput{Messages: []*schema.Message{schema.UserMessage("hello")}}
	task, err := s.SubmitNativeSubagent(ctx, actor("parent"), binding, input, "native fixture")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.SubmitNativeSubagent(ctx, actor("parent"), binding, input, "native fixture")
	if err != nil || replay.ID != task.ID {
		t.Fatalf("native replay: %+v %v", replay, err)
	}
	if _, err = s.SubmitNativeSubagent(ctx, actor("parent"), binding, input, "changed"); !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatalf("native conflicting replay: %v", err)
	}
	if err = s.manager.Execute(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	paused, err := s.Get(ctx, actor("parent"), task.ID)
	if err != nil || paused.Status != "waiting_input" {
		t.Fatalf("native pause: %+v %v", paused, err)
	}
	if _, exists, err := s.store.Get(ctx, task.ID+"/checkpoint"); err != nil || !exists {
		t.Fatalf("raw native checkpoint: %v %v", exists, err)
	}
	if opened.Load() != 1 || joined.Load() != 1 {
		t.Fatalf("attempt ownership open=%d join=%d", opened.Load(), joined.Load())
	}
	reopened, err := New(ctx, s.config)
	if err != nil {
		t.Fatal(err)
	}
	approval := harness.TaskApproval{ID: "approval", TaskVersion: paused.Version}
	if _, err = s.store.DB().Exec("CREATE TRIGGER fixture_resume_fault BEFORE UPDATE ON eino_background_tasks WHEN OLD.status='waiting_input' AND NEW.status='pending' BEGIN SELECT RAISE(ABORT,'resume fault'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.ResolveApproval(ctx, actor("parent"), task.ID, approval); err == nil {
		t.Fatal("wanted resume persistence fault")
	}
	if countRows(t, s.store, "fixture_approvals") != 0 {
		t.Fatal("approval was consumed before resume persisted")
	}
	if _, err = s.store.DB().Exec("DROP TRIGGER fixture_resume_fault"); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.ResolveApproval(ctx, actor("parent"), task.ID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.ResolveApproval(ctx, actor("parent"), task.ID, approval); !errors.Is(err, bt.ErrVersionConflict) {
		t.Fatalf("stale approval replay: %v", err)
	}
	if err = reopened.manager.Execute(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	finished, err := reopened.Get(ctx, actor("parent"), task.ID)
	if err != nil || finished.Status != "completed" {
		t.Fatalf("native resume: %+v %v", finished, err)
	}
	if opened.Load() != 2 || joined.Load() != 2 || resumed.Load() != 1 {
		t.Fatalf("resume rebuilt ownership open=%d join=%d resume=%d", opened.Load(), joined.Load(), resumed.Load())
	}
	if _, exists, err := s.store.Get(ctx, task.ID+"/checkpoint"); err != nil || exists {
		t.Fatalf("completed native checkpoint retained: %v %v", exists, err)
	}
}
