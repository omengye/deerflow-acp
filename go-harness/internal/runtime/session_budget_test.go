package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestSessionBudgetSnapshotAndIndependentRevision(t *testing.T) {
	ctx := context.Background()
	host := harness.BudgetLimits{MaxTokens: 2000, MaxModelCalls: 10, MaxToolCalls: 20, MaxOutputTokens: 100}
	s, native, ledger, x := budgetService(t, host, budget.Config{})
	request := harness.SessionBudgetRequest{SessionID: x.ID, ExpectedVersion: 1, OperationID: "set-budget", Limits: harness.TaskBudget{MaxTokens: 1000, MaxModelCalls: 2, MaxToolCalls: 3}}
	selected, err := s.SetSessionBudget(ctx, "owner", request)
	if err != nil || selected.Version != 2 || selected.Effective != request.Limits {
		t.Fatalf("select: %+v %v", selected, err)
	}
	updated, err := s.Store.Session(ctx, x.ID)
	if err != nil || updated.ConfigVersion != x.ConfigVersion || updated.BudgetVersion != 2 || updated.TaskBudget() != request.Limits {
		t.Fatalf("budget changed execution contract: %+v %v", updated, err)
	}
	reopened, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reopened.Session(ctx, x.ID)
	if err != nil || restored.TaskBudget() != request.Limits {
		t.Fatalf("budget lost across store reopen: %+v %v", restored, err)
	}
	var root string
	s.Engine = testEngine(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		root = req.RootBudgetID
		scope, _ := budget.ScopeFromContext(ctx)
		grant, err := ledger.ReserveModel(ctx, scope, budget.ModelRequest{OperationID: NewID(), Digest: "fixture", InputTokens: 50, MaxOutputTokens: 100})
		if err != nil {
			return harness.RunResult{}, err
		}
		if _, err = ledger.MarkDispatched(ctx, grant); err != nil {
			return harness.RunResult{}, err
		}
		_, err = ledger.Settle(ctx, grant, budget.Settlement{Usage: harness.Usage{InputTokens: 50, OutputTokens: 20, TotalTokens: 70}, Complete: true})
		return harness.RunResult{StopReason: "end_turn"}, err
	})
	if _, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "fixture task"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	before, err := ledger.Snapshot(ctx, root)
	if err != nil || before.Limits.MaxTokens != 1000 || before.Limits.MaxModelCalls != 2 || before.SpentTokens != 70 {
		t.Fatalf("root snapshot: %+v %v", before, err)
	}
	request.OperationID = "inherit-host"
	request.ExpectedVersion = 2
	request.Limits = harness.TaskBudget{}
	if result, err := s.SetSessionBudget(ctx, "owner", request); err != nil || result.Effective != budgetSelection(host) {
		t.Fatalf("inherit: %+v %v", result, err)
	}
	after, err := ledger.Snapshot(ctx, root)
	if err != nil || after.Limits != before.Limits || after.SpentTokens != before.SpentTokens || after.ModelCalls != before.ModelCalls {
		t.Fatalf("existing task ledger changed: %+v %v", after, err)
	}
}

func TestSessionBudgetCASIdempotenceAndRollback(t *testing.T) {
	ctx := context.Background()
	s, native, _, x := budgetService(t, harness.BudgetLimits{MaxTokens: 1000, MaxModelCalls: 10}, budget.Config{})
	req := harness.SessionBudgetRequest{SessionID: x.ID, ExpectedVersion: 1, OperationID: "first", Limits: harness.TaskBudget{MaxTokens: 500}}
	first, err := s.SetSessionBudget(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := s.Coordinator.Begin(ctx, x.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.SetSessionBudget(ctx, "owner", req)
	release()
	if err != nil || replayed != first {
		t.Fatalf("idempotent response while busy: %+v %v", replayed, err)
	}
	req.Limits.MaxTokens = 600
	if _, err = s.SetSessionBudget(ctx, "owner", req); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("reused operation identity: %v", err)
	}
	req.OperationID = "stale"
	if _, err = s.SetSessionBudget(ctx, "owner", req); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	req.ExpectedVersion = 2
	req.Limits.MaxTokens = 1001
	if _, err = s.SetSessionBudget(ctx, "owner", req); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("host cap bypassed: %v", err)
	}
	req.Limits.MaxTokens = 600
	if _, err = native.DB().Exec(`CREATE TRIGGER budget_change_fault BEFORE INSERT ON harness_session_budget_changes BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSessionBudget(ctx, "owner", req); err == nil {
		t.Fatal("operation persistence failure ignored")
	}
	current, err := s.SessionBudget(ctx, "owner", x.ID)
	if err != nil || current != first {
		t.Fatalf("partial budget update escaped rollback: %+v %v", current, err)
	}
	if _, err = s.SessionBudget(ctx, "other", x.ID); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("foreign owner: %v", err)
	}
}

func TestDeleteCommittedCleanupFailureCannotBecomeRefusal(t *testing.T) {
	ctx := context.Background()
	s, _, _, x := budgetService(t, harness.DefaultBudgetLimits(), budget.Config{})
	s.SessionCleanup = func(string) error { return harness.ErrBusy }
	if _, err := s.DeleteAttachedSession(ctx, "owner", x.ID); err == nil || errors.Is(err, harness.ErrBusy) {
		t.Fatalf("post-commit cleanup misclassified as pre-delete refusal: %v", err)
	}
	if _, err := s.Store.Session(ctx, x.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("delete failed to commit: %v", err)
	}
	s.SessionCleanup = nil
	if already, err := s.DeleteSession(ctx, x.ID); err != nil || !already {
		t.Fatalf("delete retry: %v %v", already, err)
	}
}
