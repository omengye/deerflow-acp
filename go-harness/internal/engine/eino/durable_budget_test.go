package eino

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func durableFixture(t *testing.T, id string, limits harness.BudgetLimits) (*sqlite.Store, *durablebudget.Ledger, harness.RunRequest, durablebudget.Scope) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	l, err := durablebudget.New(store.DB(), durablebudget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	req := request(id)
	req.RootBudgetID = req.RunID
	s := durablebudget.Scope{RootBudgetID: req.RootBudgetID, MemberID: req.RunID, SessionID: req.Session.ID, AttemptID: id + "/1", Fence: 1}
	if err = l.CreateRoot(context.Background(), durablebudget.RootSpec{RootBudgetID: s.RootBudgetID, RootRunID: s.MemberID, SessionID: s.SessionID, Limits: limits}); err != nil {
		t.Fatal(err)
	}
	if err = l.BeginAttempt(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return store, l, req, s
}

func TestDurableBudgetRejectsMissingOrDifferentScope(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 2}
	_, ledger, req, scope := durableFixture(t, "scope", limits)
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("unexpected"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger})
	if _, err := e.Run(context.Background(), req, nil, nil); !errors.Is(err, durablebudget.ErrScope) {
		t.Fatalf("missing scope: %v", err)
	}
	bad := req
	bad.RunID = "another-run"
	if _, err := e.Run(durablebudget.WithScope(context.Background(), scope), bad, nil, nil); !errors.Is(err, durablebudget.ErrScope) {
		t.Fatalf("wrong member: %v", err)
	}
	if fake.calls != 0 {
		t.Fatal("unbound provider invoked")
	}
}

func TestDurableBudgetCountsDeniedToolsAndSettlesAllHolds(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 40}
	_, ledger, req, scope := durableFixture(t, "denied", limits)
	underlying := &recordingTool{}
	e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger})
	if _, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.RejectOnce, nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	if underlying.calls.Load() != 0 || s.ToolCalls != 1 || s.ModelCalls != 2 || s.HeldTokens != 0 || s.SpentTokens <= 0 {
		t.Fatalf("denial accounting: %+v tools=%d", s, underlying.calls.Load())
	}
	if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
}

func TestDurableBudgetFailedResumeChargesWithOriginalCheckpoint(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 2, MaxTokens: 100000, MaxOutputTokens: 40}
	_, ledger, req, scope := durableFixture(t, "durable-resume", limits)
	started := make(chan struct{})
	failure := errors.New("provider failed after reservation")
	fake := waitingModel(started, func(context.Context) (*schema.StreamReader[*schema.Message], error) { return nil, failure })
	e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger})
	ctx, cancel := context.WithCancel(durablebudget.WithScope(context.Background(), scope))
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, req, nil, nil); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not join")
	}
	key := CheckpointID(req.RunID)
	previous, found, err := e.config.CheckpointStore.Get(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("checkpoint found=%v err=%v", found, err)
	}
	saved, err := decodeCheckpointEnvelope(previous)
	if err != nil || saved.Version != 2 || saved.Ledger == nil || saved.Budget != nil {
		t.Fatalf("durable envelope: %+v %v", saved, err)
	}
	first, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelCalls != 1 || first.SpentTokens == 0 || first.HeldTokens != 0 {
		t.Fatalf("cancel charge: %+v", first)
	}
	if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeCancelled); err != nil {
		t.Fatal(err)
	}
	scope.Fence = 2
	scope.AttemptID = req.RunID + "/2"
	if err = ledger.BeginAttempt(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(durablebudget.WithScope(context.Background(), scope), req, key, nil, nil); !errors.Is(err, failure) {
		t.Fatalf("resume failure: %v", err)
	}
	got, found, err := e.config.CheckpointStore.Get(context.Background(), key)
	if err != nil || !found || !bytes.Equal(previous, got) {
		t.Fatalf("failed resume changed bytes: found=%v err=%v", found, err)
	}
	second, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ModelCalls != 2 || second.SpentTokens <= first.SpentTokens || second.HeldTokens != 0 {
		t.Fatalf("failed retry refunded: first=%+v second=%+v", first, second)
	}
	if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeFailed); err != nil {
		t.Fatal(err)
	}
	scope.Fence = 3
	scope.AttemptID = req.RunID + "/3"
	if err = ledger.BeginAttempt(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resume(durablebudget.WithScope(context.Background(), scope), req, key, nil, nil)
	if err != nil || result.Limit != "model_calls" || fake.calls != 2 {
		t.Fatalf("old checkpoint reset quota: result=%+v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestDurableSettlementFailureSurvivesQuotaTermination(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 2, MaxTokens: 100000, MaxOutputTokens: 50}
	store, ledger, req, scope := durableFixture(t, "settlement-error", limits)
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		_, err := store.DB().Exec(`CREATE TRIGGER fail_settlement BEFORE UPDATE OF spent_tokens ON budget_roots WHEN NEW.spent_tokens>OLD.spent_tokens BEGIN SELECT RAISE(ABORT,'injected settlement failure'); END;`)
		if err != nil {
			return nil, err
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "overshoot", ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 200000, TotalTokens: 200000}}}}), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Budget: limits, BudgetLedger: ledger})
	result, err := e.Run(durablebudget.WithScope(context.Background(), scope), req, nil, nil)
	var persistence *durablebudget.PersistenceError
	if !errors.As(err, &persistence) || result.Limit != "tokens" {
		t.Fatalf("SQL failure swallowed: result=%+v err=%v", result, err)
	}
	s, err := ledger.Snapshot(context.Background(), scope.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	if s.HeldTokens <= 0 || s.SpentTokens != 0 {
		t.Fatalf("failed SQL released hold: %+v", s)
	}
	atomic := &durablebudget.PersistenceError{Operation: "settle", Err: context.DeadlineExceeded}
	filtered := withoutBudgetTermination(errors.Join(&budgetError{resource: "time"}, atomic, context.Canceled))
	if !errors.As(filtered, &persistence) {
		t.Fatalf("SQL deadline stripped: %v", filtered)
	}
	filtered = withoutNativeTermination(errors.Join(atomic, context.Canceled))
	if !errors.As(filtered, &persistence) {
		t.Fatalf("native filter stripped SQL: %v", filtered)
	}
}
