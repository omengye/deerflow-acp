package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type backgroundPermissionStub struct{ *backgroundControllerStub }

func (s *backgroundPermissionStub) BackgroundPermission(ctx context.Context, actor harness.TaskActor, q harness.BackgroundPermissionQuery) (harness.PermissionRequest, error) {
	if err := s.record(ctx, backgroundCall{Method: "permission", Actor: actor, ID: q.TaskID, Version: q.TaskVersion}); err != nil {
		return harness.PermissionRequest{}, err
	}
	if q.TaskID != "task-1" || q.InteractionID != "batch-1" || q.IntentID != "intent-1" || q.TaskVersion != 7 {
		return harness.PermissionRequest{}, harness.ErrExecutionConflict
	}
	return harness.PermissionRequest{ID: q.IntentID, ToolName: "write_file", ToolCallID: "write-1", Arguments: json.RawMessage(`{"path":"review.txt","content":"reviewable content"}`)}, nil
}

func TestACPBackgroundPermissionPreviewStrictOwnerAndFreshBatch(t *testing.T) {
	f := newFixture(t, nil)
	stub := &backgroundPermissionStub{&backgroundControllerStub{service: f.service}}
	f.service.Background = stub
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"permissionMethod":"_deerflow/tasks/permission/get"`)) {
		t.Fatalf("preview capability absent: %s", init)
	}
	session := c.newSession(t, f.cwd)
	params := map[string]any{"sessionId": session, "taskId": "task-1", "interactionId": "batch-1", "taskVersion": 7, "intentId": "intent-1"}
	method := "_deerflow/tasks/permission/get"
	other := connect(t, f.service)
	other.initialize(t)
	if denied := other.response(t, other.request(t, method, params)); denied.Error == nil || bytes.Contains(denied.Result, []byte("reviewable content")) {
		t.Fatalf("other connection received tool arguments: %+v", denied)
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`,
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","taskVersion":7,"intentId":"intent-1","targets":{}}`, session),
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","taskVersion":7,"taskVersion":8,"intentId":"intent-1"}`, session),
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","TaskVersion":7,"intentId":"intent-1"}`, session),
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","taskVersion":0,"intentId":"intent-1"}`, session),
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","taskVersion":1.5,"intentId":"intent-1"}`, session),
		fmt.Sprintf(`{"sessionId":%q,"taskId":"task-1","interactionId":"batch-1","taskVersion":7,"intentId":null}`, session),
	} {
		msg := c.response(t, c.request(t, method, json.RawMessage(raw)))
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("invalid preview accepted %s: %+v", raw, msg)
		}
	}
	if len(stub.snapshot()) != 0 {
		t.Fatal("invalid/unowned preview reached host")
	}
	params["taskVersion"] = 6
	stale := c.response(t, c.request(t, method, params))
	if stale.Error == nil || stale.Error.Code != -32021 {
		t.Fatalf("stale preview: %+v", stale)
	}
	params["taskVersion"] = 7
	preview := c.success(t, c.request(t, method, params))
	if !bytes.Contains(preview, []byte(`"path":"review.txt"`)) || !bytes.Contains(preview, []byte("reviewable content")) {
		t.Fatalf("missing reviewable operation: %s", preview)
	}
	for _, call := range stub.snapshot() {
		if call.Method != "permission" {
			t.Fatalf("preview performed a mutation: %+v", call)
		}
	}
}
