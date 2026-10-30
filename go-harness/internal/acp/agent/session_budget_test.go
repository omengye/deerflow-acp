package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"testing"
)

func TestACPSessionBudgetCapabilityBoundsAndIdempotence(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	f.service.Store.BudgetLimits = harness.BudgetLimits{MaxTokens: 1000, MaxModelCalls: 10, MaxToolCalls: 20}
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"setMethod":"_deerflow/session/budget/set"`)) {
		t.Fatalf("missing budget capability: %s", init)
	}
	id := c.newSession(t, f.cwd)
	request := map[string]any{"sessionId": id, "expectedVersion": 1, "operationId": "budget-fixture", "limits": map[string]any{"maxTokens": 500, "maxModelCalls": 2, "maxToolCalls": 0}}
	first := c.success(t, c.request(t, "_deerflow/session/budget/set", request))
	var selected harness.SessionBudget
	if json.Unmarshal(first, &selected) != nil || selected.Version != 2 || selected.Effective.MaxTokens != 500 || selected.Effective.MaxToolCalls != 20 {
		t.Fatalf("budget response: %s", first)
	}
	repeated := c.success(t, c.request(t, "_deerflow/session/budget/set", request))
	if string(first) != string(repeated) {
		t.Fatalf("idempotency: %s != %s", first, repeated)
	}
	request["operationId"] = "too-large"
	request["expectedVersion"] = 2
	request["limits"].(map[string]any)["maxTokens"] = 1001
	invalid := c.response(t, c.request(t, "_deerflow/session/budget/set", request))
	if invalid.Error == nil || invalid.Error.Code != protocol.InvalidParams {
		t.Fatalf("host limit exceeded: %+v", invalid)
	}
	request["limits"].(map[string]any)["maxTokens"] = 100
	request["limits"].(map[string]any)["resetUsage"] = true
	invalid = c.response(t, c.request(t, "_deerflow/session/budget/set", request))
	if invalid.Error == nil || invalid.Error.Code != protocol.InvalidParams {
		t.Fatalf("unknown budget authority accepted: %+v", invalid)
	}
}
