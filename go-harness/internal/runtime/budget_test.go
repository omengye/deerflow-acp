package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func budgetService(t *testing.T, limits harness.BudgetLimits, config budget.Config) (*Service, *sqlite.Store, *budget.Ledger, harness.Session) {
	t.Helper()
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := budget.New(native.DB(), config)
	if err != nil {
		t.Fatal(err)
	}
	store.BudgetLedger, store.BudgetLimits = ledger, limits
	service := NewService(store, nil, "test")
	x, err := service.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return service, native, ledger, x
}

func TestRuntimeBudgetAdmissionAndTerminalTransactions(t *testing.T) {
	ctx := context.Background()
	s, native, ledger, x := budgetService(t, harness.BudgetLimits{MaxModelCalls: 3, MaxTokens: 2000, MaxOutputTokens: 100}, budget.Config{})
	if _, err := native.DB().Exec(`CREATE TRIGGER fail_input BEFORE INSERT ON harness_inputs BEGIN SELECT RAISE(ABORT,'input failure'); END`); err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: x, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "question"}}}
	req.RootBudgetID = req.RunID
	if err := s.Store.BeginRun(ctx, req); err == nil {
		t.Fatal("admission must fail")
	}
	for _, table := range []string{"harness_inputs", "harness_runs", "budget_roots", "budget_members", "budget_attempts"} {
		var count int
		if err := native.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
	if _, err := native.DB().Exec(`DROP TRIGGER fail_input`); err != nil {
		t.Fatal(err)
	}
	var runID string
	emitFailure := errors.New("connection output failed")
	s.Engine = testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		runID = req.RunID
		scope, ok := budget.ScopeFromContext(ctx)
		if !ok || scope.RootBudgetID != req.RunID || scope != foregroundBudgetScope(req) {
			t.Fatalf("scope=%+v request=%+v", scope, req)
		}
		var count int
		if err := native.DB().QueryRow(`SELECT COUNT(*) FROM harness_inputs WHERE run_id=?`, req.RunID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("accepted input count=%d err=%v", count, err)
		}
		grant, err := ledger.ReserveModel(ctx, scope, budget.ModelRequest{OperationID: NewID(), Digest: "request", InputTokens: 50, MaxOutputTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		if first, err := ledger.MarkDispatched(ctx, grant); err != nil || !first {
			t.Fatalf("dispatch=%v %v", first, err)
		}
		if _, err := ledger.Settle(ctx, grant, budget.Settlement{Usage: harness.Usage{InputTokens: 50, OutputTokens: 20, TotalTokens: 70}, Complete: true}); err != nil {
			t.Fatal(err)
		}
		return harness.RunResult{}, emitFailure
	})
	if _, err := s.Run(ctx, "owner", x.ID, req.Input, nil, nil); !errors.Is(err, emitFailure) {
		t.Fatal(err)
	}
	snap, err := ledger.Snapshot(ctx, runID)
	if err != nil || snap.ModelCalls != 1 || snap.SpentTokens != 70 || snap.HeldTokens != 0 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	var runStatus, attemptState string
	if err := native.DB().QueryRow(`SELECT r.status,a.state FROM harness_runs r JOIN budget_attempts a ON a.id=r.id WHERE r.id=?`, runID).Scan(&runStatus, &attemptState); err != nil || runStatus != "failed" || attemptState != "ended" {
		t.Fatalf("run=%s attempt=%s err=%v", runStatus, attemptState, err)
	}

	// Terminal transaction failure must roll back the budget end together with
	// business state; it must not erase charges already settled independently.
	req.RunID, req.InputID = NewID(), NewID()
	req.RootBudgetID = req.RunID
	if err := s.Store.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := native.DB().Exec(`CREATE TRIGGER fail_terminal BEFORE UPDATE ON harness_runs BEGIN SELECT RAISE(ABORT,'terminal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Finish(ctx, req.RunID, "end_turn", nil, foregroundBudgetScope(req)); err == nil {
		t.Fatal("terminal should fail")
	}
	if err := native.DB().QueryRow(`SELECT r.status,a.state FROM harness_runs r JOIN budget_attempts a ON a.id=r.id WHERE r.id=?`, req.RunID).Scan(&runStatus, &attemptState); err != nil || runStatus != "running" || attemptState != "active" {
		t.Fatalf("rolled back run=%s attempt=%s err=%v", runStatus, attemptState, err)
	}
	if _, err := native.DB().Exec(`DROP TRIGGER fail_terminal`); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err = ledger.Snapshot(ctx, req.RunID)
	if err != nil || snap.BlockedReason != "unknown" {
		t.Fatalf("restart snapshot=%+v err=%v", snap, err)
	}
}

func TestRuntimeBudgetHeartbeatCoversCleanupAfterLimit(t *testing.T) {
	s, _, ledger, x := budgetService(t, harness.BudgetLimits{Timeout: 50 * time.Millisecond}, budget.Config{LeaseDuration: 900 * time.Millisecond})
	var runID string
	s.Engine = testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		runID = req.RunID
		select {
		case <-ctx.Done():
		case <-time.After(4 * time.Second):
			t.Fatal("budget heartbeat did not cancel")
		}
		// Cleanup intentionally outlives the original lease; heartbeat must keep
		// renewing after quota exhaustion until this join boundary completes.
		time.Sleep(1100 * time.Millisecond)
		return harness.RunResult{}, ctx.Err()
	})
	result, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "question"}}, nil, nil)
	if err != nil || result.StopReason != "cancelled" || result.Limit != "time" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	snap, err := ledger.Snapshot(context.Background(), runID)
	if err != nil || snap.BlockedReason != "time" || snap.Elapsed < time.Second {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
}

func TestRuntimePreservesHeartbeatPersistenceFailure(t *testing.T) {
	s, native, _, x := budgetService(t, harness.BudgetLimits{}, budget.Config{LeaseDuration: time.Second})
	s.Engine = testEngine(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if _, err := native.DB().Exec(`CREATE TRIGGER fail_heartbeat BEFORE UPDATE ON budget_roots BEGIN SELECT RAISE(ABORT,'heartbeat failure'); END`); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(4 * time.Second):
			t.Fatal("persistence failure did not cancel")
		}
		if _, err := native.DB().Exec(`DROP TRIGGER fail_heartbeat`); err != nil {
			t.Fatal(err)
		}
		return harness.RunResult{}, ctx.Err()
	})
	_, err := s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "question"}}, nil, nil)
	var persistence *budget.PersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("persistence error was lost: %v", err)
	}
	if cancellationOnly(&budget.PersistenceError{Operation: "test", Err: context.Canceled}) {
		t.Fatal("SQL cancellation became normal cancellation")
	}
}
