package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

type steeringEngineStub struct{}

func (steeringEngineStub) DurableExecutions() bool { return true }
func (steeringEngineStub) SteeringSupported() bool { return true }
func (steeringEngineStub) Run(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
	return harness.RunResult{StopReason: "end_turn"}, nil
}
func (s steeringEngineStub) Resume(ctx context.Context, req harness.RunRequest, _ string, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	return s.Run(ctx, req, emit, approve)
}

func steeringResponse(t *testing.T, c *client, id int) (wireMessage, []harness.SteeringRecord) {
	t.Helper()
	var notifications []harness.SteeringRecord
	for {
		message := c.read(t)
		if message.Method == "_deerflow/steering/updated" {
			var record harness.SteeringRecord
			if err := json.Unmarshal(message.Params, &record); err != nil {
				t.Fatal(err)
			}
			notifications = append(notifications, record)
			continue
		}
		if message.Method != "" || string(message.ID) != strconv.Itoa(id) {
			t.Fatalf("unexpected steering response: %+v", message)
		}
		return message, notifications
	}
}

func TestACPSteeringCapabilityValidationAndSessionScopedRecovery(t *testing.T) {
	f := newFixture(t, steeringEngineStub{})
	ledger, err := budget.New(f.db.DB(), budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.service.Store.BudgetLedger = ledger
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"submitMethod":"_deerflow/steering/submit"`)) {
		t.Fatalf("missing capability: %s", init)
	}
	sessionID := c.newSession(t, f.cwd)
	params := map[string]any{"sessionId": sessionID, "runId": "ended-run", "expectedVersion": 1, "requestId": "client-request", "text": "keep this correction"}
	response, notifications := steeringResponse(t, c, c.request(t, "_deerflow/steering/submit", params))
	var record harness.SteeringRecord
	if response.Error != nil || json.Unmarshal(response.Result, &record) != nil || record.Status != "rejected" || record.Reason != "run_not_found" || record.SessionID != sessionID {
		t.Fatalf("response=%s error=%v", response.Result, response.Error)
	}
	if len(notifications) != 1 || notifications[0].Sequence != record.Sequence {
		t.Fatalf("missing scoped notification: %+v", notifications)
	}
	duplicate, _ := steeringResponse(t, c, c.request(t, "_deerflow/steering/submit", params))
	var repeated harness.SteeringRecord
	if duplicate.Error != nil || json.Unmarshal(duplicate.Result, &repeated) != nil || repeated.Sequence != record.Sequence {
		t.Fatalf("duplicate=%s %v", duplicate.Result, duplicate.Error)
	}
	list := c.success(t, c.request(t, "_deerflow/steering/list", map[string]any{"sessionId": sessionID}))
	var restored harness.SteeringList
	if json.Unmarshal(list, &restored) != nil || len(restored.Items) != 1 || restored.Items[0].Text != "keep this correction" {
		t.Fatalf("list=%s", list)
	}
	params["approval"] = true
	invalid := c.response(t, c.request(t, "_deerflow/steering/submit", params))
	if invalid.Error == nil || invalid.Error.Code != protocol.InvalidParams {
		t.Fatalf("accepted additional authority: %+v", invalid)
	}
	delete(params, "approval")
	params["text"] = "replace original"
	conflict := c.response(t, c.request(t, "_deerflow/steering/submit", params))
	if conflict.Error == nil || conflict.Error.Code != -32013 {
		t.Fatalf("request key was rebound: %+v", conflict)
	}
}

func TestACPSteeringNotAdvertisedForUnsupportedEngine(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if bytes.Contains(init, []byte(`"steering"`)) {
		t.Fatalf("unsupported capability advertised: %s", init)
	}
	response := c.response(t, c.request(t, "_deerflow/steering/list", map[string]any{"sessionId": "anything"}))
	if response.Error == nil || response.Error.Code != protocol.MethodNotFound {
		t.Fatalf("unsupported steering did not fail closed: %+v", response)
	}
}
