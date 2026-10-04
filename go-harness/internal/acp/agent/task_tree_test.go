package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type taskTreeStub struct{ *backgroundControllerStub }

func (s *taskTreeStub) TaskTree(ctx context.Context, actor harness.TaskActor, query harness.TaskTreeQuery) (harness.TaskTreePage, error) {
	err := s.record(ctx, backgroundCall{Method: "tree", Actor: actor, ID: query.RootID, Cursor: query.Cursor, Limit: query.Limit})
	return harness.TaskTreePage{SessionID: actor.SessionID, Nodes: []harness.TaskTreeNode{{ID: "root/r", RootID: "r", RootSessionID: actor.SessionID, Mode: "root", Status: "running", Version: 9007199254740993, AllowedActions: []string{"cancel"}}}, HasMore: true, NextCursor: "next", Consistency: "live_keyset"}, err
}
func (s *taskTreeStub) TaskTreeNode(ctx context.Context, actor harness.TaskActor, root, node string) (harness.TaskTreeNode, error) {
	err := s.record(ctx, backgroundCall{Method: "tree_get", Actor: actor, ID: node, Cursor: root})
	return harness.TaskTreeNode{ID: node, RootID: root, RootSessionID: actor.SessionID, Version: 9007199254740993, Status: "running", AllowedActions: []string{"cancel"}}, err
}
func (s *taskTreeStub) CancelTaskTree(ctx context.Context, actor harness.TaskActor, request harness.TaskTreeCancelRequest) (harness.TaskTreeNode, error) {
	err := s.record(ctx, backgroundCall{Method: "tree_cancel", Actor: actor, ID: request.NodeID, Cursor: request.RootID, Version: request.ExpectedVersion})
	return harness.TaskTreeNode{ID: request.NodeID, RootID: request.RootID, RootSessionID: actor.SessionID, Version: request.ExpectedVersion + 1, Status: "cancelling", AllowedActions: []string{}}, err
}
func (s *taskTreeStub) AuthorizeBackgroundResources(ctx context.Context, actor harness.TaskActor, id string, taskVersion, resourceVersion int64) (harness.BackgroundTask, error) {
	err := s.record(ctx, backgroundCall{Method: "resources", Actor: actor, ID: id, Version: taskVersion, AfterVersion: resourceVersion})
	return harness.BackgroundTask{ID: id, SessionID: actor.SessionID, Status: "suspended", Version: taskVersion, Resources: &harness.BackgroundResources{Version: resourceVersion + 1, Status: "ready"}}, err
}

func TestACPTaskTreeCapabilitiesAndAuthority(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	stub := &taskTreeStub{&backgroundControllerStub{service: f.service}}
	f.service.Background = stub
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	for _, method := range []string{"_deerflow/tasks/tree", "_deerflow/tasks/tree/get", "_deerflow/tasks/tree/cancel", "_deerflow/tasks/resources/authorize"} {
		if !bytes.Contains(init, []byte(method)) {
			t.Fatalf("missing capability %s: %s", method, init)
		}
	}
	sid := c.newSession(t, f.cwd)
	list := c.success(t, c.request(t, "_deerflow/tasks/tree", map[string]any{"sessionId": sid, "rootId": "r", "cursor": "old", "limit": 2}))
	if !bytes.Contains(list, []byte(`"consistency":"live_keyset"`)) || !bytes.Contains(list, []byte(`"nextCursor":"next"`)) {
		t.Fatalf("lost page contract: %s", list)
	}
	c.success(t, c.request(t, "_deerflow/tasks/tree/get", map[string]any{"sessionId": sid, "rootId": "r", "nodeId": "root/r"}))
	cancel := map[string]any{"sessionId": sid, "rootId": "r", "nodeId": "root/r", "expectedVersion": int64(9007199254740993)}
	c.notify(t, "_deerflow/tasks/tree/cancel", cancel)
	result := c.success(t, c.request(t, "_deerflow/tasks/tree/cancel", cancel))
	if !bytes.Contains(result, []byte(`"status":"cancelling"`)) || !bytes.Contains(result, []byte(`"version":9007199254740994`)) {
		t.Fatalf("cancel projection lost precision or claimed completion: %s", result)
	}
	resources := map[string]any{"sessionId": sid, "taskId": "task", "taskVersion": int64(9007199254740993), "resourceVersion": int64(9007199254740995)}
	result = c.success(t, c.request(t, "_deerflow/tasks/resources/authorize", resources))
	if !bytes.Contains(result, []byte(`"status":"suspended"`)) || !bytes.Contains(result, []byte(`"version":9007199254740996`)) {
		t.Fatalf("authorization incorrectly resumed or rounded version: %s", result)
	}
	stranger := connect(t, f.service)
	stranger.initialize(t)
	for method, request := range map[string]any{"_deerflow/tasks/tree/cancel": cancel, "_deerflow/tasks/resources/authorize": resources} {
		msg := stranger.response(t, stranger.request(t, method, request))
		if msg.Error == nil {
			t.Fatalf("foreign connection accepted %s", method)
		}
	}
	calls := stub.snapshot()
	if len(calls) != 4 || calls[2].Version != 9007199254740993 || calls[3].AfterVersion != 9007199254740995 {
		t.Fatalf("wrong calls/versions: %+v", calls)
	}
	for _, call := range calls {
		if call.Actor.SessionID != sid || call.Actor.OwnerID == "" {
			t.Fatalf("missing connection authority: %+v", call)
		}
	}
	stub.mu.Lock()
	stub.err = harness.ErrExecutionConflict
	stub.mu.Unlock()
	msg := c.response(t, c.request(t, "_deerflow/tasks/tree/cancel", cancel))
	if msg.Error == nil || msg.Error.Code != -32021 || !strings.Contains(fmt.Sprint(msg.Error.Data), "_deerflow/tasks/tree/get") {
		t.Fatalf("wrong tree recovery: %+v", msg)
	}
	stub.mu.Lock()
	stub.err = harness.ErrTaskTreeStopped
	stub.mu.Unlock()
	msg = c.response(t, c.request(t, "_deerflow/tasks/tree/cancel", cancel))
	if msg.Error == nil || msg.Error.Code != -32021 || !strings.Contains(fmt.Sprint(msg.Error.Data), "task_stopped") {
		t.Fatalf("stopped tree lost request-local error: %+v", msg)
	}
	stub.mu.Lock()
	stub.err = errors.Join(harness.ErrTaskTreeStopped, harness.ErrBackgroundUncertain)
	stub.mu.Unlock()
	msg = c.response(t, c.request(t, "_deerflow/tasks/tree/cancel", cancel))
	if msg.Error == nil || msg.Error.Code != -32022 || !strings.Contains(fmt.Sprint(msg.Error.Data), "task_reconciliation_required") {
		t.Fatalf("cancellation hid uncertain effects: %+v", msg)
	}
	// A management error is request-local and does not poison ordinary prompts.
	c.success(t, c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []map[string]string{{"type": "text", "text": "continue"}}}))
}

func TestACPTaskTreeStrictParametersAndOptionalCapabilities(t *testing.T) {
	f := newFixture(t, nil)
	stub := &taskTreeStub{&backgroundControllerStub{service: f.service}}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for _, test := range []struct{ method, body string }{
		{"_deerflow/tasks/tree", `"cursor":null`},
		{"_deerflow/tasks/tree", `"limit":1.5`},
		{"_deerflow/tasks/tree", `"limit":101`},
		{"_deerflow/tasks/tree", `"RootID":"r"`},
		{"_deerflow/tasks/tree", `"rootId":"r","rootId":"other"`},
		{"_deerflow/tasks/tree/get", `"rootId":"r","nodeId":null`},
		{"_deerflow/tasks/tree/get", `"rootId":"r","nodeId":"root/r","ownerId":"spoof"`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r"`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r","expectedVersion":0`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r","expectedVersion":null`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r","expectedVersion":1e0`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r","expectedVersion":"1"`},
		{"_deerflow/tasks/tree/cancel", `"rootId":"r","nodeId":"root/r","expectedVersion":1,"scope":"node"`},
		{"_deerflow/tasks/resources/authorize", `"taskId":"task","taskVersion":1,"resourceVersion":null`},
		{"_deerflow/tasks/resources/authorize", `"taskId":"task","taskVersion":1,"resourceVersion":-1`},
		{"_deerflow/tasks/resources/authorize", `"taskId":"task","taskVersion":1,"resourceVersion":1.5`},
		{"_deerflow/tasks/resources/authorize", `"taskId":"task","taskVersion":1,"resourceVersion":9223372036854775808`},
		{"_deerflow/tasks/resources/authorize", `"taskId":"task","taskVersion":1,"resourceVersion":1,"generation":"supplied"`},
	} {
		msg := c.response(t, c.request(t, test.method, json.RawMessage(fmt.Sprintf(`{"sessionId":%q,%s}`, sid, test.body))))
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("accepted %s %s: %+v", test.method, test.body, msg)
		}
	}
	if len(stub.snapshot()) != 0 {
		t.Fatal("malformed request reached controller")
	}
	unsupported := newFixture(t, nil)
	unsupported.service.Background = &backgroundControllerStub{service: unsupported.service}
	other := connect(t, unsupported.service)
	init := other.success(t, other.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if bytes.Contains(init, []byte(`"taskTree"`)) || bytes.Contains(init, []byte(`"resourceAuthorizeMethod"`)) {
		t.Fatalf("unsupported methods advertised: %s", init)
	}
	for _, method := range []string{"_deerflow/tasks/tree", "_deerflow/tasks/tree/get", "_deerflow/tasks/tree/cancel", "_deerflow/tasks/resources/authorize"} {
		msg := other.response(t, other.request(t, method, map[string]any{"sessionId": sid}))
		if msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
			t.Fatalf("optional method %s available: %+v", method, msg)
		}
	}
}
