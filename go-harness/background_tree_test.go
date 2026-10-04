package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func TestSDKNestedBackgroundRolesKeepTreeAndSharedLimits(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("durable_wait_%t", waiting), func(t *testing.T) { runNestedBackgroundTree(t, waiting, false, false) })
	}
	t.Run("restart_with_leaf_permission", func(t *testing.T) { runNestedBackgroundTree(t, true, true, false) })
	t.Run("mixed_wait_and_permission", func(t *testing.T) { runNestedBackgroundTree(t, true, false, true) })
}

func runNestedBackgroundTree(t *testing.T, waiting, restart, mixed bool) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                           `json:"model"`
			Stream   bool                             `json:"stream"`
			Messages []struct{ Role, Content string } `json:"messages"`
			Tools    []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		hasResult, canSubmit := false, false
		var lastTask harness.BackgroundTask
		for _, m := range request.Messages {
			hasResult = hasResult || m.Role == "tool"
			if m.Role == "tool" {
				_ = json.Unmarshal([]byte(m.Content), &lastTask)
			}
		}
		for _, tool := range request.Tools {
			canSubmit = canSubmit || tool.Function.Name == "background_agent"
		}
		next := map[string]string{"parent": "one", "one": "two", "two": "three"}[request.Model]
		message := map[string]any{"role": "assistant", "content": "finished " + request.Model}
		finish := "stop"
		if next != "" && !hasResult {
			if !canSubmit {
				t.Errorf("model %s has no background capability", request.Model)
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "submit-" + next, "type": "function", "function": map[string]string{"name": "background_agent", "arguments": fmt.Sprintf(`{"role":%q,"instruction":"isolated work"}`, next)}}}
			finish = "tool_calls"
		}
		if waiting && request.Model != "parent" && next != "" && hasResult && lastTask.Status != "completed" {
			if lastTask.ID == "" || lastTask.Version < 1 {
				t.Errorf("invalid submitted task: %+v", lastTask)
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "wait-" + request.Model, "type": "function", "function": map[string]string{"name": "task_wait", "arguments": fmt.Sprintf(`{"taskId":%q,"afterVersion":%d}`, lastTask.ID, lastTask.Version)}}}
			finish = "tool_calls"
			if mixed && request.Model == "one" {
				message["tool_calls"] = append(message["tool_calls"].([]any), map[string]any{"index": 1, "id": "write-mixed", "type": "function", "function": map[string]string{"name": "write_file", "arguments": `{"path":"mixed.txt","content":"once"}`}})
			}
		}
		if restart && request.Model == "three" && !hasResult {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "leaf-write", "type": "function", "function": map[string]string{"name": "write_file", "arguments": `{"path":"leaf.txt","content":"once"}`}}}
			finish = "tool_calls"
		}
		if request.Model == "three" && canSubmit {
			t.Error("leaf role can exceed max depth")
		}
		writeTreeModelResponse(w, request.Stream, message, finish)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	depth, concurrency, taskLimit := 3, 1, 3
	limits := harness.BudgetLimits{MaxModelCalls: 12, MaxToolCalls: 8, MaxTokens: 100000, MaxOutputTokens: 128}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeOff, Budget: &limits,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{MaxDepth: &depth, MaxConcurrent: &concurrency, MaxTasksPerRoot: &taskLimit, CustomAgents: map[string]harness.AgentConfig{}},
	}
	if restart || mixed {
		cfg.PermissionMode = harness.PermissionModeDangerous
	}
	for _, name := range []string{"one", "two", "three"} {
		cfg.ModelRoutes[name] = harness.ModelRoute{Provider: "openai", Model: name, BaseURL: provider.URL + "/v1", APIKey: "fixture"}
		role := harness.AgentConfig{Description: name, SystemPrompt: "Complete this role's work", Model: name}
		if name == "one" {
			role.DelegateTo = []string{"two"}
		}
		if name == "two" {
			role.DelegateTo = []string{"three"}
		}
		cfg.Subagents.CustomAgents[name] = role
	}
	client, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	workspace := t.TempDir()
	session, err := client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var tasks []harness.BackgroundTask
	reopened := false
	for {
		tasks, err = client.BackgroundTasks(ctx, session.ID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		complete := len(tasks) == 3
		if restart && !reopened && len(tasks) == 3 {
			paused := true
			for _, task := range tasks {
				paused = paused && task.Status == "waiting_input" && task.Interaction != nil
			}
			if paused {
				if err = client.Close(); err != nil {
					t.Fatal(err)
				}
				client = nil
				// Removed current definitions must not replace archived recipes.
				cfg.Subagents.CustomAgents = map[string]harness.AgentConfig{}
				client, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = client.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				reopened = true
			}
		}
		for _, task := range tasks {
			if task.Status == "failed" || task.Status == "canceled" {
				t.Fatalf("nested task ended unsuccessfully: %+v", task)
			}
			complete = complete && task.Status == "completed"
			if task.Status == "waiting_input" && task.Interaction != nil && task.Interaction.Resumable {
				if restart && !reopened && task.Depth == 3 {
					continue
				}
				if _, err = client.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tree did not complete: %+v", tasks)
		case <-time.After(15 * time.Millisecond):
		}
	}
	for _, task := range tasks {
		var data []byte
		var spec hr.BackgroundExecutionSpec
		if err = client.store.DB().QueryRowContext(ctx, `SELECT payload FROM harness_background_specs WHERE task_id=?`, task.ID).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		if spec.Version != 5 || task.RootSessionID != session.ID || spec.Depth != task.Depth || spec.ParentTaskID != task.ParentTaskID {
			t.Fatalf("lost tree recipe: %+v %+v", task, spec)
		}
		if waiting && !restart && !mixed && task.Depth < 3 && task.Attempt != 2 {
			t.Fatalf("wait did not release and resume worker: %+v", task)
		}
		var invocation agents.Invocation
		if err = client.store.DB().QueryRowContext(ctx, `SELECT root_id,parent_id,depth FROM harness_agent_invocations WHERE id=?`, "background/"+task.ID).Scan(&invocation.RootID, &invocation.ParentID, &invocation.Depth); err != nil {
			t.Fatal(err)
		}
		wantParent := ""
		if task.ParentTaskID != "" {
			wantParent = "background/" + task.ParentTaskID
		}
		if invocation.RootID != result.Execution.RunID || invocation.ParentID != wantParent || invocation.Depth != task.Depth {
			t.Fatalf("role registration lost ancestry: %+v", invocation)
		}
	}
	if restart && !reopened {
		t.Fatal("restart scenario did not restart")
	}
	path := ""
	if restart {
		path = "leaf.txt"
	}
	if mixed {
		path = "mixed.txt"
	}
	if path != "" {
		data, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || string(data) != "once" {
			t.Fatalf("approved effect: %q %v", data, err)
		}
		var writes int
		if err = client.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE state='completed' AND json_extract(receipt,'$.toolName')='write_file'`).Scan(&writes); err != nil || writes != 1 {
			t.Fatalf("write receipts=%d %v", writes, err)
		}
	}
}

func writeTreeModelResponse(w http.ResponseWriter, stream bool, message map[string]any, finish string) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	} else {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}
}
