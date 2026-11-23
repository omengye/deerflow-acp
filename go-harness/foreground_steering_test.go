package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPForegroundSteeringAfterToolApproval(t *testing.T) {
	const promptID = "desktop-prompt-1"
	const correction = "Report only the contents of approved.txt."
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	afterTool, releaseModel := make(chan struct{}), make(chan struct{})
	var modelCalls, approvals atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		call := modelCalls.Add(1)
		copies := 0
		for _, message := range request.Messages {
			var text string
			if message.Role == "user" && json.Unmarshal(message.Content, &text) == nil && text == correction {
				copies++
			}
		}
		if call < 3 && copies != 0 || call == 3 && copies != 1 {
			t.Errorf("model request %d contains %d copies of the correction", call, copies)
		}
		if call >= 3 {
			writeTreeModelResponse(w, request.Stream, map[string]any{"role": "assistant", "content": "done"}, "stop")
			return
		}
		tool, args, id := "write_file", `{"path":"approved.txt","content":"approved"}`, "write"
		if call == 2 {
			close(afterTool)
			select {
			case <-releaseModel:
			case <-ctx.Done():
				return
			}
			tool, args, id = "read_file", `{"path":"approved.txt"}`, "read"
		}
		writeTreeModelResponse(w, request.Stream, map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": tool, "arguments": args}},
		}}, "tool_calls")
	}))
	t.Cleanup(provider.Close)
	limits := harness.BudgetLimits{MaxModelCalls: 4, MaxToolCalls: 4, MaxTokens: 100000, MaxOutputTokens: 256}
	client, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", Budget: &limits,
		APIKey: "fixture", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeDangerous})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	type binding struct{ SessionID, PromptID, RunID string }
	targets := make(chan binding, 4)
	peer := connectBackgroundACP(t, ctx, client, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "_deerflow/steering/target":
			var target binding
			if err := json.Unmarshal(raw, &target); err != nil {
				return nil, err
			}
			targets <- target
		case "session/request_permission":
			approvals.Add(1)
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
		}
		return nil, nil
	})
	workspace := t.TempDir()
	var session struct{ SessionID string }
	if err := peer.Call(ctx, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}}, &session); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var prompt continuationACPResponse
	go func() {
		done <- peer.Call(ctx, "session/prompt", map[string]any{"sessionId": session.SessionID,
			"prompt": []harness.Content{{Type: "text", Text: "Write approved.txt and verify it."}},
			"_meta":  map[string]any{"deerflow": map[string]string{"promptId": promptID}}}, &prompt)
	}()
	select {
	case <-afterTool:
	case err := <-done:
		t.Fatalf("prompt finished before the tool checkpoint: %v", err)
	case <-ctx.Done():
		t.Fatalf("waiting for approved tool: model calls=%d approvals=%d: %v", modelCalls.Load(), approvals.Load(), ctx.Err())
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "approved.txt")); err != nil || string(data) != "approved" || approvals.Load() == 0 {
		t.Fatalf("approved tool did not run: approvals=%d data=%q err=%v", approvals.Load(), data, err)
	}
	var target binding
	select {
	case target = <-targets:
	case <-time.After(3 * time.Second):
		t.Fatal("foreground session/prompt ran through tool approval without publishing its steering target")
	}
	if target.SessionID != session.SessionID || target.PromptID != promptID || target.RunID == "" {
		t.Fatalf("wrong foreground binding: %+v", target)
	}
	var state harness.ExecutionState
	scope := map[string]any{"sessionId": target.SessionID, "runId": target.RunID}
	if err := peer.Call(ctx, "_deerflow/executions/get", scope, &state); err != nil {
		t.Fatal(err)
	}
	var accepted harness.SteeringRecord
	if err := peer.Call(ctx, "_deerflow/steering/submit", map[string]any{"sessionId": target.SessionID, "runId": target.RunID,
		"expectedVersion": state.Version, "requestId": "3c637177-257a-4d76-ac7d-0c2bb5ffce53", "text": correction}, &accepted); err != nil || accepted.Status != "accepted" {
		t.Fatalf("submit after approval: %+v, %v", accepted, err)
	}
	close(releaseModel)
	if err := <-done; err != nil || prompt.StopReason != "end_turn" || prompt.Meta.Deerflow.Execution == nil || prompt.Meta.Deerflow.Execution.RunID != target.RunID {
		t.Fatalf("original prompt: %+v %v", prompt, err)
	}
	var records harness.SteeringList
	if err := peer.Call(ctx, "_deerflow/steering/list", scope, &records); err != nil || len(records.Items) != 1 || records.Items[0].Status != "applied" ||
		records.Items[0].RequestID != accepted.RequestID || records.Items[0].RunID != target.RunID || modelCalls.Load() != 3 {
		t.Fatalf("final steering records: %+v, calls=%d err=%v", records, modelCalls.Load(), err)
	}
}
