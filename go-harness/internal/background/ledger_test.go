package background

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func realLedgerFixture(t *testing.T, limits harness.BudgetLimits, clock func() time.Time, executor *fixtureExecutor) (*Service, *budget.Ledger, *LedgerAdapter) {
	t.Helper()
	var service *Service
	var ledger *budget.Ledger
	var adapter *LedgerAdapter
	service, _ = newFixture(t, func(c *Config) {
		var err error
		ledger, err = budget.New(c.Store.DB(), budget.Config{Now: clock, LeaseDuration: 20 * time.Second, CheckEffectTx: func(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
			if scope.MemberID == "run-1" {
				return nil
			}
			return service.CheckBudgetEffectTx(ctx, tx, scope)
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = ledger.CreateRoot(context.Background(), budget.RootSpec{RootBudgetID: "root-1", RootRunID: "run-1", SessionID: "parent", Limits: limits}); err != nil {
			t.Fatal(err)
		}
		adapter, err = NewLedgerAdapter(c.Store, ledger)
		if err != nil {
			t.Fatal(err)
		}
		c.Budgets = adapter
		if executor != nil {
			c.AdditionalExecutors = []bt.Executor{executor}
		}
	})
	return service, ledger, adapter
}

func TestLedgerAdapterRejectsForgedRootOriginAndChildScope(t *testing.T) {
	s, ledger, adapter := realLedgerFixture(t, harness.BudgetLimits{}, nil, nil)
	ctx := context.Background()
	if err := ledger.CreateRoot(ctx, budget.RootSpec{RootBudgetID: "other-root", RootRunID: "other-run", SessionID: "parent"}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BindMember(ctx, budget.MemberBinding{RootBudgetID: "root-1", MemberID: "sibling", SessionID: "sibling-child", OriginRunID: "run-1", ParentMemberID: "run-1", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Submission){
		func(in *Submission) { in.Binding.RootBudgetID = "other-root" },
		func(in *Submission) { in.Binding.OriginRunID = "other-run" },
		func(in *Submission) { in.Binding.OriginRunID = "sibling" },
	} {
		in := submission("parent", "forged")
		mutate(&in)
		if _, err := s.Submit(ctx, actor("parent"), in); !errors.Is(err, budget.ErrScope) {
			t.Fatalf("forged binding accepted: %+v %v", in.Binding, err)
		}
	}
	if countRows(t, s.store, "eino_background_tasks") != 0 {
		t.Fatal("budget binding rejection left native task")
	}
	task := submitFixture(t, s, submission("parent", "valid"))
	_, attemptCtx, state := claimFixture(t, s, task.ID)
	scope := BudgetScope(state.scope)
	if scope.RootBudgetID != "root-1" || scope.MemberID != task.ID || scope.SessionID != task.ChildSessionID || scope.Fence != 1 || scope.AttemptID != task.ID+"/attempt/1" {
		t.Fatalf("scope mismatch: %+v", scope)
	}
	if err := adapter.BeforeAttempt(attemptCtx, state.scope); err != nil {
		t.Fatal(err)
	}
	bad := state.scope
	bad.Binding.ChildSessionID = "forged-child"
	if err := adapter.BeforeAttempt(attemptCtx, bad); !errors.Is(err, budget.ErrScope) {
		t.Fatalf("forged child attempt: %v", err)
	}
	wrong := scope
	wrong.SessionID = "forged-child"
	if _, err := ledger.ReserveTool(attemptCtx, wrong, budget.ToolRequest{OperationID: "forged", Digest: "x"}); !errors.Is(err, budget.ErrScope) {
		t.Fatalf("forged model budget scope: %v", err)
	}
}

func TestLedgerAdapterSettlesBeforeNativeOutcome(t *testing.T) {
	for _, settle := range []bool{true, false} {
		name := "unsettled"
		if settle {
			name = "settled"
		}
		t.Run(name, func(t *testing.T) {
			var s *Service
			var ledger *budget.Ledger
			executor := &fixtureExecutor{run: func(ctx context.Context, task *bt.Task, _ bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
				scope, ok := ScopeFromContext(ctx)
				if !ok {
					return nil, budget.ErrScope
				}
				grant, err := ledger.ReserveModel(ctx, BudgetScope(scope), budget.ModelRequest{OperationID: task.Spec.ID + "/model", Digest: "fixture", InputTokens: 10, MaxOutputTokens: 20})
				if err != nil {
					return nil, err
				}
				if _, err = ledger.MarkDispatched(ctx, grant); err != nil {
					return nil, err
				}
				if settle {
					if _, err = ledger.Settle(ctx, grant, budget.Settlement{Complete: true, Usage: harness.Usage{TotalTokens: 12}}); err != nil {
						return nil, err
					}
					return &bt.ExecutionResult{Status: bt.StatusCompleted}, nil
				}
				if err = s.CheckpointsForAttempt().Set(ctx, task.Spec.ID+"/checkpoint", []byte("unsafe-new")); err != nil {
					return nil, err
				}
				return &bt.ExecutionResult{Status: bt.StatusWaitingInput, Checkpoint: []byte("new-metadata")}, nil
			}}
			s, ledger, _ = realLedgerFixture(t, harness.BudgetLimits{MaxTokens: 100}, nil, executor)
			task := submitFixture(t, s, submission("parent", "task"))
			ctx := context.Background()
			if err := s.store.Set(ctx, task.ID+"/checkpoint", []byte("prior-safe")); err != nil {
				t.Fatal(err)
			}
			err := s.manager.Execute(ctx, task.ID)
			if settle && err != nil {
				t.Fatal(err)
			}
			if !settle && !errors.Is(err, budget.ErrUnknown) {
				t.Fatalf("unsettled gate: %v", err)
			}
			got, err := s.Get(ctx, actor("parent"), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := ledger.Snapshot(ctx, "root-1")
			if err != nil {
				t.Fatal(err)
			}
			if settle {
				if got.Status != "completed" || snap.SpentTokens != 12 || snap.HeldTokens != 0 {
					t.Fatalf("settled outcome: %+v %+v", got, snap)
				}
			} else {
				if got.Status != "running" || snap.HeldTokens != 30 {
					t.Fatalf("unsafe checkpoint published: %+v %+v", got, snap)
				}
				data, _, err := s.store.Get(ctx, task.ID+"/checkpoint")
				if err != nil || string(data) != "prior-safe" {
					t.Fatalf("checkpoint advanced before settlement: %s %v", data, err)
				}
			}
		})
	}
}

func TestNativeHeartbeatAndBudgetClockCommitTogetherThroughCleanup(t *testing.T) {
	var nanos atomic.Int64
	nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	s, ledger, adapter := realLedgerFixture(t, harness.BudgetLimits{Timeout: 5 * time.Second}, now, nil)
	task := submitFixture(t, s, submission("parent", "task"))
	native, ctx, state := claimFixture(t, s, task.ID)
	if err := adapter.BeforeAttempt(ctx, state.scope); err != nil {
		t.Fatal(err)
	}
	var initialLease int64
	if err := s.store.DB().QueryRow("SELECT lease_ns FROM budget_attempts WHERE id=?", BudgetScope(state.scope).AttemptID).Scan(&initialLease); err != nil {
		t.Fatal(err)
	}
	nanos.Add(int64(10 * time.Second))
	if _, err := s.store.DB().Exec("CREATE TRIGGER fixture_native_heartbeat_fault BEFORE UPDATE ON eino_background_tasks BEGIN SELECT RAISE(ABORT,'native heartbeat fault'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.tasks.Heartbeat(ctx, &bt.HeartbeatRequest{TaskID: task.ID, ExpectedVersion: native.Version}); err == nil {
		t.Fatal("wanted native heartbeat fault")
	}
	var lease, elapsed int64
	if err := s.store.DB().QueryRow("SELECT lease_ns FROM budget_attempts WHERE id=?", BudgetScope(state.scope).AttemptID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if err := s.store.DB().QueryRow("SELECT elapsed_ns FROM budget_roots WHERE id='root-1'").Scan(&elapsed); err != nil {
		t.Fatal(err)
	}
	if lease != initialLease || elapsed != 0 {
		t.Fatalf("budget heartbeat escaped native rollback: lease=%d elapsed=%d", lease, elapsed)
	}
	if _, err := s.store.DB().Exec("DROP TRIGGER fixture_native_heartbeat_fault"); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.tasks.Heartbeat(ctx, &bt.HeartbeatRequest{TaskID: task.ID, ExpectedVersion: native.Version})
	if err != nil {
		t.Fatalf("quota exhaustion rolled back heartbeat: %v", err)
	}
	if _, err = s.Cancel(ctx, actor("parent"), task.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	current, err := s.tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version <= renewed.Version {
		t.Fatal("cancel intent did not persist")
	}
	if err = s.CheckEffect(ctx); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("cancel allowed new effects: %v", err)
	}
	nanos.Add(int64(15 * time.Second))
	if _, err = s.tasks.Heartbeat(ctx, &bt.HeartbeatRequest{TaskID: task.ID, ExpectedVersion: current.Version}); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("native heartbeat after cancel must retain upstream semantics: %v", err)
	}
	state.mu.Lock()
	state.ready = true
	state.joined = true
	state.mu.Unlock()
	if _, err = s.tasks.AckCancel(ctx, &bt.AckCancelRequest{TaskID: task.ID, ExpectedVersion: current.Version, Reason: "stop"}); err != nil {
		t.Fatal(err)
	}
	nanos.Add(int64(time.Hour))
	snap, err := ledger.Snapshot(context.Background(), "root-1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Elapsed != 25*time.Second || snap.BlockedReason != "time" {
		t.Fatalf("cleanup lifetime did not count exactly: %+v", snap)
	}
	var ended string
	if err = s.store.DB().QueryRow("SELECT state FROM budget_attempts WHERE id=?", BudgetScope(state.scope).AttemptID).Scan(&ended); err != nil || ended != "ended" {
		t.Fatalf("budget attempt still live after task terminal: %s %v", ended, err)
	}
}

func TestBudgetMonitorStopsBlockedFactoryAndExecutor(t *testing.T) {
	for _, phase := range []string{"factory", "executor"} {
		t.Run(phase, func(t *testing.T) {
			var entered, joined atomic.Bool
			executor := &fixtureExecutor{run: func(ctx context.Context, _ *bt.Task, _ bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
				entered.Store(true)
				<-ctx.Done()
				return &bt.ExecutionResult{Status: bt.StatusCompleted}, nil
			}}
			s, ledger, _ := realLedgerFixture(t, harness.BudgetLimits{Timeout: 40 * time.Millisecond}, nil, executor)
			s.config.Attempts = &fixtureFactory{open: func(ctx context.Context, _ TaskScope) (*Attempt, error) {
				if phase == "factory" {
					entered.Store(true)
					<-ctx.Done()
				}
				return &Attempt{JoinAndClose: func(context.Context) error { time.Sleep(80 * time.Millisecond); joined.Store(true); return nil }}, nil
			}}
			task := submitFixture(t, s, submission("parent", "blocked"))
			done := make(chan error, 1)
			go func() { done <- s.manager.Execute(context.Background(), task.ID) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("budget monitor did not stop blocked operation")
			}
			got, err := s.Get(context.Background(), actor("parent"), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !entered.Load() || !joined.Load() || got.Status != "failed" || !strings.Contains(got.Error, "time budget exhausted") {
				t.Fatalf("budget cancellation lost outcome/join: entered=%v joined=%v task=%+v", entered.Load(), joined.Load(), got)
			}
			snap, err := ledger.Snapshot(context.Background(), "root-1")
			if err != nil {
				t.Fatal(err)
			}
			if snap.Elapsed < 100*time.Millisecond || snap.BlockedReason != "time" {
				t.Fatalf("cleanup excluded from active budget time: %+v", snap)
			}
		})
	}
}

type failingMonitor struct {
	BudgetLedger
	HeartbeatBudgetLedger
}

func (failingMonitor) CheckAttemptBudget(context.Context, TaskScope) error {
	return &budget.PersistenceError{Operation: "monitor fixture", Err: errors.New("unavailable accounting storage")}
}

func TestBudgetMonitorPreservesPersistenceFailureWhenExecutorSwallowsCancel(t *testing.T) {
	executor := &fixtureExecutor{run: func(ctx context.Context, _ *bt.Task, _ bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
		<-ctx.Done()
		return &bt.ExecutionResult{Status: bt.StatusCompleted}, nil
	}}
	s, _, adapter := realLedgerFixture(t, harness.BudgetLimits{}, nil, executor)
	s.config.Budgets = failingMonitor{BudgetLedger: adapter, HeartbeatBudgetLedger: adapter}
	task := submitFixture(t, s, submission("parent", "blocked"))
	if err := s.manager.Execute(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), actor("parent"), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || !strings.Contains(got.Error, "budget monitor fixture persistence") || !strings.Contains(got.Error, "unavailable accounting storage") {
		t.Fatalf("monitor persistence fault became successful cancellation: %+v", got)
	}
}
