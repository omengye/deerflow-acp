package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func connectBackgroundACP(t *testing.T, ctx context.Context, client *Client, handler protocol.Handler) *protocol.Peer {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	peer := protocol.NewPeer(clientIn, clientOut, handler, protocol.Options{})
	serverDone, clientDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- client.ServeACP(ctx, serverIn, serverOut) }()
	go func() { clientDone <- peer.Serve(ctx) }()
	t.Cleanup(func() {
		_ = peer.Close()
		for _, done := range []chan error{clientDone, serverDone} {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("ACP pipe did not join after close")
			}
		}
	})
	var initialize json.RawMessage
	if err := peer.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, &initialize); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(initialize), `"approveMethod":"_deerflow/tasks/approve"`) {
		t.Fatalf("real host omitted background capability: %s", initialize)
	}
	if !strings.Contains(string(initialize), `"permissionMethod":"_deerflow/tasks/permission/get"`) {
		t.Fatalf("real host omitted saved permission preview capability: %s", initialize)
	}
	return peer
}

func TestACPRealBackgroundHostApprovalAndNotificationOwnership(t *testing.T) {
	var parentCalls, childCalls, permissionCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		child, hasTool := false, false
		for _, message := range request.Messages {
			child = child || message.Role == "user" && strings.Contains(string(message.Content), "ACP_CHILD_INSTRUCTION")
			hasTool = hasTool || message.Role == "tool"
		}
		if child {
			childCalls.Add(1)
		} else {
			parentCalls.Add(1)
		}
		name, id, args := "background_agent", "acp-delegate", `{ "instruction": "ACP_CHILD_INSTRUCTION: write child.txt", "description": "ACP child work" }`
		if child {
			name, id, args = "write_file", "acp-child-write", `{ "path": "child.txt", "content": "approved through ACP" }`
		}
		message := map[string]any{"role": "assistant", "content": "Work finished"}
		finish := "stop"
		if !hasTool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if !hasTool {
			raw, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, raw)
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Work finished\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	limits := harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 2, MaxTokens: 100000, MaxOutputTokens: 256}
	client, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, Budget: &limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	peer := connectBackgroundACP(t, ctx, client, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "session/update" {
			return nil, nil
		}
		if method != "session/request_permission" {
			return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "unexpected callback"}
		}
		var permission struct {
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		}
		if err := json.Unmarshal(raw, &permission); err != nil || permission.ToolCall.Title != "background_agent" {
			t.Errorf("foreground callback received child permission: %s %v", raw, err)
			return nil, errors.New("unexpected child permission callback")
		}
		permissionCalls.Add(1)
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
	})
	workspace := t.TempDir()
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := peer.Call(ctx, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}}, &session); err != nil || session.ID == "" {
		t.Fatalf("new session=%+v err=%v", session, err)
	}
	var prompt struct {
		StopReason string `json:"stopReason"`
	}
	if err := peer.Call(ctx, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "Delegate the work to a child"}}}, &prompt); err != nil || prompt.StopReason != "end_turn" {
		t.Fatalf("parent prompt=%+v err=%v", prompt, err)
	}
	var listed struct {
		Tasks []harness.BackgroundTask `json:"tasks"`
	}
	if err := peer.Call(ctx, "_deerflow/tasks/list", map[string]any{"sessionId": session.ID, "limit": 10}, &listed); err != nil || len(listed.Tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", listed.Tasks, err)
	}
	task := listed.Tasks[0]
	params := map[string]any{"sessionId": session.ID, "taskId": task.ID}
	if err := peer.Call(ctx, "_deerflow/tasks/get", params, &task); err != nil {
		t.Fatal(err)
	}
	waitTerminal := func() {
		t.Helper()
		for task.Status == "pending" || task.Status == "running" {
			if err := peer.Call(ctx, "_deerflow/tasks/wait", map[string]any{"sessionId": session.ID, "taskId": task.ID, "afterVersion": task.Version}, &task); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitTerminal()
	if task.Status != "waiting_input" || task.Interaction == nil || !task.Interaction.Resumable || len(task.Interaction.WaitingInputs) != 1 || task.Interaction.WaitingInputs[0].ToolName != "write_file" {
		t.Fatalf("real child did not pause for approval: %+v", task)
	}
	if _, err := os.Stat(filepath.Join(workspace, "child.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child wrote before approval: %v", err)
	}
	// A second ACP connection cannot use the first connection's attached session.
	other := connectBackgroundACP(t, ctx, client, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	assertRPCCode := func(err error, code int) {
		t.Helper()
		var wire *protocol.Error
		if !errors.As(err, &wire) || wire.Code != code {
			t.Fatalf("wire error=%v want code=%d", err, code)
		}
	}
	assertRPCCode(other.Call(ctx, "_deerflow/tasks/get", params, nil), protocol.InvalidParams)
	previewParams := map[string]any{"sessionId": session.ID, "taskId": task.ID, "interactionId": task.Interaction.ID, "taskVersion": task.Version, "intentId": task.Interaction.WaitingInputs[0].ID}
	var preview harness.PermissionRequest
	if err := peer.Call(ctx, "_deerflow/tasks/permission/get", previewParams, &preview); err != nil {
		t.Fatal(err)
	}
	var previewArgs map[string]string
	if err := json.Unmarshal(preview.Arguments, &previewArgs); err != nil || preview.ToolName != "write_file" || preview.ToolCallID != "acp-child-write" || preview.SessionID != task.ChildSessionID || len(previewArgs) != 2 || previewArgs["path"] != "child.txt" || previewArgs["content"] != "approved through ACP" {
		t.Fatalf("saved ACP permission preview=%+v args=%v err=%v", preview, previewArgs, err)
	}
	assertRPCCode(other.Call(ctx, "_deerflow/tasks/permission/get", previewParams, nil), protocol.InvalidParams)
	previewParams["taskVersion"] = task.Version - 1
	assertRPCCode(peer.Call(ctx, "_deerflow/tasks/permission/get", previewParams, nil), -32021)
	if _, err := os.Stat(filepath.Join(workspace, "child.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("permission preview executed child tool: %v", err)
	}
	approval := map[string]any{"sessionId": session.ID, "taskId": task.ID, "approval": map[string]any{"id": task.Interaction.ID, "taskVersion": task.Version - 1, "decision": "allow_once"}}
	assertRPCCode(peer.Call(ctx, "_deerflow/tasks/approve", approval, nil), -32021)
	approval["approval"] = map[string]any{"id": task.Interaction.ID, "taskVersion": task.Version, "decision": "allow_once"}
	assertRPCCode(other.Call(ctx, "_deerflow/tasks/approve", approval, nil), protocol.InvalidParams)
	if err := peer.Call(ctx, "_deerflow/tasks/approve", approval, &task); err != nil {
		t.Fatal(err)
	}
	waitTerminal()
	if task.Status != "completed" {
		t.Fatalf("approved child=%+v", task)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
	if err != nil || string(data) != "approved through ACP" {
		t.Fatalf("child output=%q err=%v", data, err)
	}
	assertRPCCode(peer.Call(ctx, "_deerflow/tasks/approve", approval, nil), -32021)
	if parentCalls.Load() != 2 || childCalls.Load() != 2 || permissionCalls.Load() != 1 {
		t.Fatalf("calls parent=%d child=%d foreground permissions=%d", parentCalls.Load(), childCalls.Load(), permissionCalls.Load())
	}
	var notifications struct {
		Items []harness.BackgroundNotification `json:"notifications"`
	}
	for len(notifications.Items) == 0 {
		if err := peer.Call(ctx, "_deerflow/notifications/list", map[string]any{"sessionId": session.ID, "after": 0, "limit": 100}, &notifications); err != nil {
			t.Fatal(err)
		}
		if len(notifications.Items) == 0 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	notification := notifications.Items[0]
	if notification.TaskID != task.ID || notification.SessionID != session.ID {
		t.Fatalf("misrouted notification=%+v", notification)
	}
	assertRPCCode(other.Call(ctx, "_deerflow/notifications/list", map[string]any{"sessionId": session.ID}, nil), protocol.InvalidParams)
	ack := map[string]any{"sessionId": session.ID, "notificationId": notification.ID}
	assertRPCCode(other.Call(ctx, "_deerflow/notifications/ack", ack, nil), protocol.InvalidParams)
	if err := peer.Call(ctx, "_deerflow/notifications/ack", ack, nil); err != nil {
		t.Fatal(err)
	}
	if err := peer.Call(ctx, "_deerflow/notifications/list", map[string]any{"sessionId": session.ID, "limit": 100}, &notifications); err != nil {
		t.Fatal(err)
	}
	for _, remaining := range notifications.Items {
		if remaining.ID == notification.ID {
			t.Fatal("owner acknowledgement was not persisted")
		}
	}
}
