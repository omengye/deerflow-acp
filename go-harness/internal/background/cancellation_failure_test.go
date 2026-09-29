package background

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func cancelledLateFailureFixture(t *testing.T, late error) (*Service, harness.BackgroundTask, <-chan error, chan struct{}) {
	t.Helper()
	started, finish := make(chan struct{}), make(chan struct{})
	executor := &fixtureExecutor{run: func(context.Context, *bt.Task, bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
		close(started)
		<-finish
		return &bt.ExecutionResult{Status: bt.StatusCanceled, Error: "user stopped"}, nil
	}}
	s, _, _ := realLedgerFixture(t, harness.BudgetLimits{}, nil, executor)
	s.config.Attempts = &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
		return &Attempt{JoinAndClose: func(context.Context) error { return nil }, ExecutionFailure: func() error { return late }}, nil
	}}
	task := submitFixture(t, s, submission("parent", "late-cancel"))
	done := make(chan error, 1)
	go func() { done <- s.manager.Execute(context.Background(), task.ID) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not start")
	}
	if _, err := s.Cancel(context.Background(), actor("parent"), task.ID, "user stopped"); err != nil {
		t.Fatal(err)
	}
	return s, task, done, finish
}

func waitCancellationFixture(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not join")
		return nil
	}
}

func TestCancellationPreservesLatePersistenceFailureWithoutQuarantine(t *testing.T) {
	for name, late := range map[string]error{
		"ordinary_cancel": nil,
		"context_cancel":  context.Canceled,
		"persistence":     &budget.PersistenceError{Operation: "late settlement", Err: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			s, task, done, finish := cancelledLateFailureFixture(t, late)
			close(finish)
			if err := waitCancellationFixture(t, done); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(context.Background(), actor("parent"), task.ID)
			if err != nil || got.Status != "canceled" || got.BlockedReason != "" || !strings.Contains(got.Error, "user stopped") {
				t.Fatal("cancel changed outcome or quarantined joined resources", got, err)
			}
			wantDiagnostic := name == "persistence"
			if strings.Contains(got.Error, "late settlement") != wantDiagnostic {
				t.Fatal("late persistence error lost or ordinary cancellation reported as a fault", got)
			}
			var state, outcome string
			if err = s.store.DB().QueryRow(`SELECT state,outcome FROM budget_attempts WHERE member_id=?`, task.ID).Scan(&state, &outcome); err != nil || state != "ended" || outcome != "cancelled" {
				t.Fatal("cancel retained active budget attempt", state, outcome, err)
			}
			if countRows(t, s.store, "harness_background_execution_failures") != map[bool]int{true: 1, false: 0}[wantDiagnostic] {
				t.Fatal("incorrect diagnostic rows", got)
			}
			if wantDiagnostic {
				var persistence bool
				if err = s.store.DB().QueryRow(`SELECT persistence_failure FROM harness_background_execution_failures WHERE task_id=?`, task.ID).Scan(&persistence); err != nil || !persistence {
					t.Fatal("persistence classification lost", persistence, err)
				}
			}
			if countRows(t, s.store, "harness_background_child_leases") != 0 {
				t.Fatal("joined cancellation retained child lease")
			}
			// Reassemble the service to verify diagnostics are not process memory.
			reopened, err := New(context.Background(), s.config)
			if err != nil {
				t.Fatal(err)
			}
			listed, err := reopened.List(context.Background(), actor("parent"), "", 10)
			if err != nil || len(listed) != 1 || listed[0].Error != got.Error {
				t.Fatal("reassembled service lost execution diagnostic", listed, err)
			}
		})
	}
}

func TestCancellationFailureDiagnosticAndLedgerRollbackTogether(t *testing.T) {
	for _, fault := range []string{"diagnostic", "ledger"} {
		t.Run(fault, func(t *testing.T) {
			late := &budget.PersistenceError{Operation: "late receipt", Err: errors.New("receipt unavailable")}
			s, task, done, finish := cancelledLateFailureFixture(t, late)
			trigger := `CREATE TRIGGER cancellation_fault BEFORE INSERT ON harness_background_execution_failures BEGIN SELECT RAISE(ABORT,'diagnostic fault'); END`
			if fault == "ledger" {
				trigger = `CREATE TRIGGER cancellation_fault BEFORE UPDATE ON budget_attempts WHEN NEW.state='ended' BEGIN SELECT RAISE(ABORT,'ledger end fault'); END`
			}
			if _, err := s.store.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			close(finish)
			if err := waitCancellationFixture(t, done); err == nil {
				t.Fatal("failed terminal transaction reported success")
			}
			if countRows(t, s.store, "harness_background_execution_failures") != 0 || countRows(t, s.store, "harness_background_child_leases") != 1 {
				t.Fatal("terminal failure partly committed diagnostic or released lease")
			}
			var state string
			if err := s.store.DB().QueryRow(`SELECT state FROM budget_attempts WHERE member_id=?`, task.ID).Scan(&state); err != nil || state != "active" {
				t.Fatal("failed transaction partly ended ledger", state, err)
			}
			pending, err := s.tasks.Get(context.Background(), task.ID)
			if err != nil || pending.Status != bt.StatusRunning || pending.CancelRequestedAt == nil {
				t.Fatal("failed diagnostic advanced native cancellation", pending, err)
			}
			if _, err = s.store.DB().Exec(`DROP TRIGGER cancellation_fault`); err != nil {
				t.Fatal(err)
			}
			if _, err = s.tasks.AckCancel(context.Background(), &bt.AckCancelRequest{TaskID: task.ID, ExpectedVersion: pending.Version, Reason: "user stopped"}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(context.Background(), actor("parent"), task.ID)
			if err != nil || got.Status != "canceled" || !strings.Contains(got.Error, "late receipt") || got.BlockedReason != "" {
				t.Fatal("retry could not complete joined cancellation", got, err)
			}
		})
	}
}

func TestJoinedDrainCheckpointFailureSettlesRealLedger(t *testing.T) {
	late := &budget.PersistenceError{Operation: "drain checkpoint", Err: errors.New("checkpoint storage failed")}
	joined := errors.Join(bt.ErrDrainCheckpointUnavailable, late)
	wrapped := &joinedDrainFailure{cause: joined}
	var actual *budget.PersistenceError
	if errors.Is(wrapped, bt.ErrDrainCheckpointUnavailable) || !errors.Is(wrapped, late) || !errors.As(wrapped, &actual) || actual != late {
		t.Fatal("drain normalization lost actual persistence cause")
	}
	s, _, _ := realLedgerFixture(t, harness.BudgetLimits{}, nil, &fixtureExecutor{run: func(context.Context, *bt.Task, bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
		return nil, joined
	}})
	s.config.Attempts = &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
		return &Attempt{JoinAndClose: func(context.Context) error { return nil }}, nil
	}}
	task := submitFixture(t, s, submission("parent", "joined-drain-failure"))
	if err := s.manager.Execute(context.Background(), task.ID); err != nil {
		t.Fatal("joined failure should commit native failed state", err)
	}
	got, err := s.Get(context.Background(), actor("parent"), task.ID)
	if err != nil || got.Status != "failed" || got.BlockedReason != "" || !strings.Contains(got.Error, late.Error()) {
		t.Fatal("drain failure was lost or quarantined", got, err)
	}
	var state, outcome string
	if err = s.store.DB().QueryRow(`SELECT state,outcome FROM budget_attempts WHERE member_id=?`, task.ID).Scan(&state, &outcome); err != nil || state != "ended" || outcome != "failed" {
		t.Fatal("drain left a running ledger attempt", state, outcome, err)
	}
	if countRows(t, s.store, "harness_background_child_leases") != 0 {
		t.Fatal("joined drain failure retained child lease")
	}
}

func TestWorkerRetainsJoinedAttemptAfterTerminalCommitFailure(t *testing.T) {
	late := &budget.PersistenceError{Operation: "joined provider receipt", Err: errors.New("receipt fault")}
	s, _, _ := realLedgerFixture(t, harness.BudgetLimits{}, nil, nil)
	s.config.Attempts = &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
		return &Attempt{JoinAndClose: func(context.Context) error { return nil }, ExecutionFailure: func() error { return late }}, nil
	}}
	reported := make(chan error, 10)
	s.config.OnError = func(err error) { reported <- err }
	if _, err := s.store.DB().Exec(`CREATE TRIGGER terminal_fault BEFORE UPDATE ON budget_attempts WHEN NEW.state='ended' BEGIN SELECT RAISE(ABORT,'terminal fault'); END`); err != nil {
		t.Fatal(err)
	}
	task := submitFixture(t, s, submission("parent", "worker-terminal-fault"))
	if err := s.StartWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "terminal fault") {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not report terminal commit fault")
	}
	s.mu.Lock()
	var retained *attemptState
	for _, state := range s.attempts {
		if state.scope.Binding.TaskID == task.ID {
			retained = state
		}
	}
	s.mu.Unlock()
	if retained == nil || !retained.isJoined() {
		t.Fatal("worker discarded joined state before its terminal transaction committed")
	}
	if countRows(t, s.store, "harness_background_execution_failures") != 0 || countRows(t, s.store, "harness_background_child_leases") != 1 {
		t.Fatal("failed terminal transaction partly committed")
	}
	if _, err := s.store.DB().Exec(`DROP TRIGGER terminal_fault`); err != nil {
		t.Fatal(err)
	}
	pending, err := s.tasks.Get(context.Background(), task.ID)
	if err != nil || pending.Status != bt.StatusRunning {
		t.Fatal(pending, err)
	}
	if _, err = s.tasks.Fail(context.Background(), &bt.FailTaskRequest{TaskID: task.ID, ExpectedVersion: pending.Version, Error: late.Error()}); err != nil {
		t.Fatal(err)
	}
	var state, outcome string
	if err = s.store.DB().QueryRow(`SELECT state,outcome FROM budget_attempts WHERE member_id=?`, task.ID).Scan(&state, &outcome); err != nil || state != "ended" || outcome != "failed" {
		t.Fatal("retry skipped retained ledger state", state, outcome, err)
	}
	if countRows(t, s.store, "harness_background_execution_failures") != 1 || countRows(t, s.store, "harness_background_child_leases") != 0 {
		t.Fatal("retry lost diagnostic or retained child lease")
	}
}

func TestBudgetMonitorDistinguishesDurableCancellationFromLeaseAndDatabaseFaults(t *testing.T) {
	for _, mutation := range []string{"cancel", "no_cancel", "expired_lease", "lost_child", "changed_attempt", "database_fault"} {
		t.Run(mutation, func(t *testing.T) {
			s, _, adapter := realLedgerFixture(t, harness.BudgetLimits{}, nil, nil)
			task := submitFixture(t, s, submission("parent", mutation))
			native, ctx, state := claimFixture(t, s, task.ID)
			if err := adapter.BeforeAttempt(ctx, state.scope); err != nil {
				t.Fatal(err)
			}
			if mutation != "no_cancel" {
				if _, err := s.tasks.RequestCancel(ctx, &bt.RequestCancelRequest{TaskID: task.ID, ExpectedVersion: native.Version, Reason: "stop"}); err != nil {
					t.Fatal(err)
				}
			}
			var statement string
			switch mutation {
			case "expired_lease":
				statement = `UPDATE eino_background_tasks SET lease_expires_at=1`
			case "lost_child", "no_cancel":
				statement = `DELETE FROM harness_background_child_leases`
			case "changed_attempt":
				state.scope.Attempt++
			case "database_fault":
				statement = `ALTER TABLE budget_roots RENAME TO unavailable_budget_roots`
			}
			if statement != "" {
				if _, err := s.store.DB().Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			err := adapter.CheckAttemptBudget(ctx, state.scope)
			if mutation == "cancel" {
				if err != context.Canceled {
					t.Fatalf("durable cancellation became diagnostic: %v", err)
				}
			} else if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("real fence/database fault was suppressed: %v", err)
			}
			if mutation == "database_fault" {
				var persistence *budget.PersistenceError
				if !errors.As(err, &persistence) || !strings.Contains(err.Error(), "budget_roots") {
					t.Fatalf("database error classification lost: %v", err)
				}
			}
		})
	}
}
