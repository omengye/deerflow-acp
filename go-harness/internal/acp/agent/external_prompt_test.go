package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestACPExternalPromptReconciliationExtension(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	var sessionID string
	f.service.ExternalPromptPending = func(ctx context.Context, owner, id, agent string) (harness.ExternalPromptState, error) {
		if owner == "" || id != sessionID || agent != "fixture" {
			t.Errorf("wrong pending scope owner=%q session=%q agent=%q", owner, id, agent)
		}
		return harness.ExternalPromptState{Agent: agent, SessionID: "remote-1", PromptID: "pending-1", RunID: "run-1", ToolCallID: "call-1"}, nil
	}
	f.service.ExternalPromptAcknowledge = func(ctx context.Context, owner, id, agent, promptID string) error {
		if owner == "" || id != sessionID || agent != "fixture" {
			t.Errorf("wrong acknowledgement scope owner=%q session=%q agent=%q", owner, id, agent)
		}
		if promptID != "pending-1" {
			return harness.ErrReconciliationRequired
		}
		return nil
	}
	c := connect(t, f.service)
	c.initialize(t)
	sessionID = c.newSession(t, f.cwd)
	id := c.request(t, "_deerflow/external_prompt/pending", map[string]any{"sessionId": sessionID, "agent": "fixture"})
	var listed struct {
		Pending harness.ExternalPromptState `json:"pending"`
	}
	if err := json.Unmarshal(c.success(t, id), &listed); err != nil || listed.Pending.PromptID != "pending-1" {
		t.Fatalf("pending=%+v err=%v", listed, err)
	}
	wrong := c.request(t, "_deerflow/external_prompt/acknowledge", map[string]any{"sessionId": sessionID, "agent": "fixture", "promptId": "other"})
	if msg := c.response(t, wrong); msg.Error == nil || msg.Error.Code != -32010 {
		t.Fatalf("unreviewed acknowledgement=%+v", msg)
	}
	ack := c.request(t, "_deerflow/external_prompt/acknowledge", map[string]any{"sessionId": sessionID, "agent": "fixture", "promptId": "pending-1"})
	var result map[string]string
	if err := json.Unmarshal(c.success(t, ack), &result); err != nil || result["acknowledged"] != "pending-1" {
		t.Fatalf("ack=%+v err=%v", result, err)
	}
	duplicate := c.request(t, "_deerflow/external_prompt/pending", json.RawMessage(`{"sessionId":"`+sessionID+`","agent":"fixture","agent":"fixture"}`))
	if msg := c.response(t, duplicate); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("duplicate identity accepted: %+v", msg)
	}
}
