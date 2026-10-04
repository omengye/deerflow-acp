package runtime

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestAutomaticExecutionRetryTransactionAndManualRecovery(t *testing.T) {
	f := newExecutionWaitFixture(t)
	state := f.suspend(t, false)
	f.complete(t)
	ctx := context.WithValue(context.Background(), automaticExecutionKey{}, true)
	claim := func(tx *sql.Tx) (ExecutionLease, error) {
		lease, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
		if err == nil {
			err = f.ledger.BeginAttemptTx(ctx, tx, lease.Scope)
		}
		return lease, err
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER automatic_fault BEFORE INSERT ON harness_execution_audits WHEN NEW.kind='attempt_resuming' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error { _, err := claim(tx); return err })
	if err == nil {
		t.Fatal("injected claim failure succeeded")
	}
	var count int
	if err = f.s.db.QueryRow(`SELECT COUNT(*) FROM harness_execution_wakeups`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback spent a retry: %d %v", count, err)
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER automatic_fault`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		var lease ExecutionLease
		err = executionTx(ctx, f.s, func(tx *sql.Tx) error { var err error; lease, err = claim(tx); return err })
		if err != nil {
			t.Fatal(err)
		}
		mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
			if _, err := f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopFailed, "retry fixture"); err != nil {
				return err
			}
			return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeWaitingInput)
		})
		// Startup recovery must preserve the charge and the original checkpoint.
		mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error { _, err := f.s.RecoverExecutionsTx(ctx, tx); return err })
		state, err = f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
		if err != nil || state.Automatic == nil || state.Automatic.Attempts != i {
			t.Fatalf("retry %d: %+v %v", i, state, err)
		}
		if i < 3 && (state.Automatic.RetryAfter == nil || !state.Automatic.RetryAfter.After(time.Now())) {
			t.Fatal("missing retry backoff")
		}
		if err = executionTx(ctx, f.s, func(tx *sql.Tx) error { _, err := claim(tx); return err }); !errors.Is(err, errAutomaticDeferred) {
			t.Fatalf("backoff/limit bypass: %v", err)
		}
		if _, err = f.s.db.Exec(`UPDATE harness_execution_wakeups SET next_at=0`); err != nil {
			t.Fatal(err)
		}
	}
	if state.Automatic.Status != "blocked" {
		t.Fatalf("retry limit: %+v", state.Automatic)
	}
	manual := f.resume(t, "new-owner")
	if manual.Scope.Fence != state.Attempt+1 || manual.Scope.RootBudgetID != f.req.RootBudgetID || manual.InputID != f.req.InputID {
		t.Fatalf("manual recovery identity: %+v", manual)
	}
}

func TestAutomaticExecutionStopAlsoGatesPermissionOnlyClaim(t *testing.T) {
	f := newExecutionFixture(t)
	f.pending(t)
	state := f.suspend(t)
	ctx := context.WithValue(context.Background(), automaticExecutionKey{}, true)
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, f.req.Session.ID, f.req.RunID)
		if err != nil {
			return err
		}
		return stopAutomaticRootTx(ctx, tx, row)
	})
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		_, err := f.s.ClaimExecutionResumeTx(ctx, tx, f.req.Session.ID, "owner", harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, NewID())
		return err
	})
	if !errors.Is(err, errAutomaticDeferred) {
		t.Fatalf("stopped root resumed: %v", err)
	}
}

type automaticWaitEngine struct {
	resumes  atomic.Int32
	onResume func(context.Context, harness.RunRequest) error
}

func (*automaticWaitEngine) DurableExecutions() bool { return true }
func (*automaticWaitEngine) Run(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
	return harness.RunResult{}, errors.New("automatic work created new input")
}
func (e *automaticWaitEngine) Resume(ctx context.Context, req harness.RunRequest, key string, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
	e.resumes.Add(1)
	if e.onResume != nil {
		if err := e.onResume(ctx, req); err != nil {
			return harness.RunResult{}, err
		}
	}
	hooks := interaction.FromContext(ctx)
	if len(hooks.WaitTargets) != 1 {
		return harness.RunResult{}, errors.New("lost durable wait targets")
	}
	return harness.RunResult{StopReason: "end_turn"}, hooks.StageCheckpoint(ctx, interaction.StagedExecutionCheckpoint{ID: key, Remove: true})
}

func automaticService(t *testing.T, f *executionWaitFixture, engine *automaticWaitEngine) *Service {
	t.Helper()
	s := NewService(f.s, engine, f.req.Session.Model)
	if _, err := s.Coordinator.Attach(f.req.Session.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	return s
}

func subscribeAutomatic(t *testing.T, s *Service, owner, sessionID string, handlers harness.SessionUpdates) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.ServeSessionUpdates(ctx, owner, sessionID, handlers) }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		registered := s.sessionUpdates[sessionID] != nil
		s.mu.Unlock()
		if registered {
			return done
		}
		select {
		case err := <-done:
			t.Fatalf("subscription exited: %v", err)
		case <-deadline:
			t.Fatal("subscription not registered")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAutomaticSessionUpdatesCancelIdleWaitAndJoin(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	engine := &automaticWaitEngine{}
	s := automaticService(t, f, engine)
	done := subscribeAutomatic(t, s, "owner", f.req.Session.ID, harness.SessionUpdates{})
	if err := s.ServeSessionUpdates(context.Background(), "owner", f.req.Session.ID, harness.SessionUpdates{}); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("duplicate subscription: %v", err)
	}
	if err := s.Cancel(context.Background(), "owner", f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	f.complete(t)
	time.Sleep(250 * time.Millisecond)
	state, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionCancelled || engine.resumes.Load() != 0 {
		t.Fatalf("cancel revived: %+v %v", state, err)
	}
	var stopped int
	if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_automatic_stops WHERE root_id=?`, f.req.RootBudgetID).Scan(&stopped); err != nil || stopped != 1 {
		t.Fatalf("stop fence: %d %v", stopped, err)
	}
	if err = s.CloseSession(context.Background(), "owner", f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticSessionUpdatesDisconnectAndReplacementOwner(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	entered := make(chan struct{})
	var oldResults atomic.Int32
	engine := &automaticWaitEngine{onResume: func(ctx context.Context, req harness.RunRequest) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	s := automaticService(t, f, engine)
	done := subscribeAutomatic(t, s, "owner", f.req.Session.ID, harness.SessionUpdates{Result: func(context.Context, harness.RunResult, error) error { oldResults.Add(1); return nil }})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic resume stalled")
	}
	if err := s.Disconnect(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionWaitingInput || oldResults.Load() != 0 {
		t.Fatalf("disconnect lost wait/output: %+v %v results=%d", state, err, oldResults.Load())
	}
	engine.onResume = func(_ context.Context, req harness.RunRequest) error {
		if req.RunID != f.req.RunID || req.InputID != f.req.InputID || req.RootBudgetID != f.req.RootBudgetID {
			return errors.New("changed accepted input")
		}
		return nil
	}
	if _, err = s.Load(context.Background(), "replacement", f.req.Session.ID, f.req.Session.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	results := make(chan harness.RunResult, 1)
	done = subscribeAutomatic(t, s, "replacement", f.req.Session.ID, harness.SessionUpdates{Result: func(_ context.Context, result harness.RunResult, err error) error {
		if err != nil {
			return err
		}
		results <- result
		return nil
	}})
	select {
	case result := <-results:
		if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || engine.resumes.Load() != 2 {
			t.Fatalf("replacement result: %+v", result)
		}
	case err := <-done:
		t.Fatalf("subscription failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not wake")
	}
	if err = s.CloseSession(context.Background(), "replacement", f.req.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticSessionCloseJoinsResultCallback(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	s := automaticService(t, f, &automaticWaitEngine{})
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := subscribeAutomatic(t, s, "owner", f.req.Session.ID, harness.SessionUpdates{Result: func(ctx context.Context, _ harness.RunResult, _ error) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil
	}})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.CloseSession(context.Background(), "owner", f.req.Session.ID) }()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not cancelled")
	}
	select {
	case err := <-closed:
		t.Fatalf("closed before callback joined: %v", err)
	default:
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticRunQueueYieldsToUserAndReservesOnce(t *testing.T) {
	q, err := newRunQueue(RunQueueConfig{MaxActiveRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	release, err := q.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user := make(chan func(), 1)
	go func() { r, _ := q.acquire(ctx); user <- r }()
	deadline := time.After(time.Second)
	for q.queued.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("user never queued")
		case <-time.After(time.Millisecond):
		}
	}
	if _, _, err = q.tryAutomatic(ctx); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic work bypassed user queue: %v", err)
	}
	release()
	release = <-user
	release()
	reserved, release, err := q.tryAutomatic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	inner, err := q.acquire(reserved)
	if err != nil {
		t.Fatal(err)
	}
	inner()
	if _, err = q.acquire(reserved); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic reservation reused: %v", err)
	}
	if len(q.slots) != 1 {
		t.Fatal("inner release dropped outer slot")
	}
}
