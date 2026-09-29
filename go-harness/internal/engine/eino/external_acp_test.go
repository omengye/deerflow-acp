package eino

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	acpclient "github.com/omengye/deerflow-acp/go-harness/internal/acp/client"
)

type externalToolFixture struct{ calls atomic.Int32 }

func (*externalToolFixture) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "invoke_acp_agent", Desc: "test external delegate", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"agent": {Type: schema.String}, "prompt": {Type: schema.String}})}, nil
}
func (f *externalToolFixture) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	f.calls.Add(1)
	callbacks, ok := acpclient.CallbacksFromContext(ctx)
	if !ok || callbacks.Update == nil {
		return "", context.Canceled
	}
	if err := callbacks.Update(ctx, json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"delegated"}}`)); err != nil {
		return "", err
	}
	return "delegated", nil
}

func TestExternalDelegationUsesParentModelQuotaAndProgress(t *testing.T) {
	for _, limit := range []int{2, 3} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			fixture := &externalToolFixture{}
			fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate-1", Type: "function", Function: schema.FunctionCall{Name: "invoke_acp_agent", Arguments: `{"agent":"fixture","prompt":"do work"}`}}}}}), nil
				}
				return textStream("done"), nil
			}}
			budget := harness.DefaultBudgetLimits()
			budget.MaxModelCalls = limit
			engine := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{fixture}, Budget: budget})
			var events []harness.RunEvent
			result, err := engine.Run(context.Background(), request("external-budget"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if fixture.calls.Load() != 1 {
				t.Fatalf("external calls=%d", fixture.calls.Load())
			}
			found := false
			for _, event := range events {
				if event.Kind == "tool_update" && len(event.Content) > 0 && event.Content[0].Text == "delegated" {
					found = true
				}
			}
			if !found {
				t.Fatal("external progress was not forwarded")
			}
			if limit == 2 && result.Limit != "model_calls" {
				t.Fatalf("budget not shared: %+v", result)
			}
			if limit == 3 && result.StopReason != "end_turn" {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
