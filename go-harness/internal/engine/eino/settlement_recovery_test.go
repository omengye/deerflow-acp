package eino

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestRuntimeModelSettlementFailureDoesNotStrandExecution(t *testing.T) {
	t.Run("completed", func(t *testing.T) { testRuntimeUnsettledModel(t, false) })
	t.Run("cancelled_during_permission", func(t *testing.T) { testRuntimeUnsettledModel(t, true) })
}

func testRuntimeUnsettledModel(t *testing.T, cancelAtTool bool) {
	t.Helper()
	var native *sqlite.Store
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		_, err := native.DB().Exec(`CREATE TRIGGER fail_model_settlement BEFORE UPDATE OF spent_tokens ON budget_roots WHEN NEW.spent_tokens>OLD.spent_tokens BEGIN SELECT RAISE(ABORT,'injected settlement failure'); END;`)
		if err != nil {
			return nil, err
		}
		if cancelAtTool {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "pending", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		}
		return textStream("answer before settlement failed"), nil
	}}
	var effects atomic.Int32
	s, session, store := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "unexpected", nil }}, false)
	native = store
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emit := func(_ context.Context, event harness.RunEvent) error {
		if cancelAtTool && event.Kind == "tool_start" {
			cancel()
		}
		return nil
	}
	result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "reply"}}, emit, nil)
	var persistence *durablebudget.PersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("settlement failure was hidden: result=%+v err=%v", result, err)
	}
	state, readErr := s.Execution(context.Background(), "owner", session.ID, "")
	if readErr != nil || state.Status != harness.ExecutionNeedsReconciliation || state.Resumable {
		t.Fatalf("failed model settlement stranded execution: state=%+v err=%v runErr=%v", state, readErr, err)
	}
	if result.Execution == nil || result.Execution.Status != state.Status {
		t.Fatalf("response did not report persisted recovery state: %+v", result)
	}
	var status, attemptState, outcome string
	if err := native.DB().QueryRow(`SELECT status FROM harness_runs WHERE id=?`, state.RunID).Scan(&status); err != nil || status != "needs_reconciliation" {
		t.Fatalf("run was left active: status=%s err=%v", status, err)
	}
	if err := native.DB().QueryRow(`SELECT state,outcome FROM budget_attempts WHERE id=?`, state.AttemptID).Scan(&attemptState, &outcome); err != nil || attemptState != "unknown" || outcome != "interrupted" {
		t.Fatalf("attempt was left active: state=%s outcome=%s err=%v", attemptState, outcome, err)
	}
	var held, spent int64
	var blocked string
	if err := native.DB().QueryRow(`SELECT held_tokens,spent_tokens,blocked_reason FROM budget_roots WHERE id=?`, state.RunID).Scan(&held, &spent, &blocked); err != nil || held <= 0 || spent != 0 || blocked != "unknown" {
		t.Fatalf("unsettled usage was refunded or made replayable: held=%d spent=%d blocked=%s err=%v", held, spent, blocked, err)
	}
	if fake.calls != 1 {
		t.Fatalf("failed model call was replayed: calls=%d", fake.calls)
	}
	if effects.Load() != 0 {
		t.Fatal("unapproved tool executed during cancellation")
	}
}
