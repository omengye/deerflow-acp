package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type liveExternalApprovalEngine struct{ decision harness.PermissionDecision }

func (*liveExternalApprovalEngine) DurableExecutions() bool { return true }
func (e *liveExternalApprovalEngine) Run(ctx context.Context, request harness.RunRequest, _ harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	if approve == nil {
		return harness.RunResult{}, harness.ErrPermissionDenied
	}
	decision, err := approve(ctx, harness.PermissionRequest{ID: request.RunID + "/outer/external/nested", SessionID: request.Session.ID, RunID: request.RunID, ConfigVersion: request.Session.ConfigVersion, ToolCallID: "outer/nested", ToolName: "external_acp/fixture/write_file", Arguments: json.RawMessage(`{"path":"report.txt"}`)})
	e.decision = decision
	return harness.RunResult{StopReason: "end_turn"}, err
}
func (e *liveExternalApprovalEngine) Resume(ctx context.Context, req harness.RunRequest, _ string, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	return e.Run(ctx, req, emit, approve)
}

func TestDurableRunRoutesLiveExternalPermissionToOwner(t *testing.T) {
	service, _, _, session := budgetService(t, harness.BudgetLimits{MaxToolCalls: 2}, budget.Config{})
	engine := &liveExternalApprovalEngine{}
	service.Engine = engine
	called := 0
	result, err := service.Run(context.Background(), "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		called++
		if p.SessionID != session.ID || p.ToolName != "external_acp/fixture/write_file" {
			t.Fatalf("wrong external permission: %+v", p)
		}
		return harness.AllowOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || called != 1 || engine.decision != harness.AllowOnce {
		t.Fatalf("result=%+v called=%d decision=%s err=%v", result, called, engine.decision, err)
	}
}
