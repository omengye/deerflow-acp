package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Real Eino checkpoints must let a stopped inner role unwind without approving
// its pending write, while preserving the enclosing native background attempt.
func TestSDKTaskTreeNativePausedRoleCancellationAcrossRestart(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                           `json:"model"`
			Stream   bool                             `json:"stream"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		hasResult := false
		for _, m := range request.Messages {
			hasResult = hasResult || m.Role == "tool"
		}
		message := map[string]any{"role": "assistant", "content": "finished " + request.Model}
		finish := "stop"
		if !hasResult {
			name, args := "background_agent", `{"role":"outer","instruction":"delegate inner work"}`
			if request.Model == "outer" {
				name, args = "task", `{"subagent_type":"inner","prompt":"write file","description":"inner work"}`
			}
			if request.Model == "inner" {
				name, args = "write_file", `{"path":"cancelled.txt","content":"must not execute"}`
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-" + request.Model, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		writeTreeModelResponse(w, request.Stream, message, finish)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	depth, concurrency, taskLimit := 2, 1, 2
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeDangerous,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{MaxDepth: &depth, MaxConcurrent: &concurrency, MaxTasksPerRoot: &taskLimit, CustomAgents: map[string]harness.AgentConfig{}}}
	for _, name := range []string{"outer", "inner"} {
		cfg.ModelRoutes[name] = harness.ModelRoute{Provider: "openai", Model: name, APIKey: "fixture", BaseURL: provider.URL + "/v1"}
		role := harness.AgentConfig{Description: name, SystemPrompt: "Complete the work", Model: name}
		if name == "outer" {
			role.DelegateTo = []string{"inner"}
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
	result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if p.ToolName != "background_agent" {
			t.Errorf("unexpected root approval %s", p.ToolName)
		}
		return harness.AllowOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var task harness.BackgroundTask
	for {
		tasks, e := client.BackgroundTasks(ctx, session.ID, "", 10)
		if e != nil {
			t.Fatal(e)
		}
		if len(tasks) == 1 {
			task = tasks[0]
			if task.Status == "waiting_input" {
				break
			}
			if task.Status == "failed" {
				t.Fatalf("native task failed: %+v", task)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never paused: %+v", task)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if task.Interaction == nil || len(task.Interaction.WaitingInputs) != 1 {
		t.Fatalf("permission=%+v", task)
	}
	oldApproval := harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}
	page, err := client.TaskTree(ctx, session.ID, harness.TaskTreeQuery{RootID: result.Execution.RunID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var inner harness.TaskTreeNode
	for _, node := range page.Nodes {
		if node.Role == "inner" {
			inner = node
		}
	}
	if inner.ID == "" || inner.Mode != "foreground" {
		t.Fatalf("inner missing: %+v", page)
	}
	stopped, err := client.CancelTaskTree(ctx, session.ID, harness.TaskTreeCancelRequest{RootID: inner.RootID, NodeID: inner.ID, ExpectedVersion: inner.Version})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.BackgroundExecution == nil || !stopped.BackgroundExecution.Resumable || stopped.BackgroundExecution.TaskID != task.ID {
		t.Fatalf("no exact cleanup reference: %+v", stopped)
	}
	if _, err = client.ApproveBackgroundTask(ctx, session.ID, task.ID, oldApproval); err == nil {
		t.Fatal("stale approval accepted after subtree stop")
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	client = nil
	client, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	task, err = client.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Interaction == nil || task.Interaction.Kind != "cancelled_roles" || !task.Interaction.Resumable || len(task.Interaction.WaitingInputs) != 0 || task.Interaction.ID != stopped.BackgroundExecution.InteractionID {
		t.Fatalf("cleanup manifest changed across restart: %+v", task)
	}
	task, err = client.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	for task.Status == "running" || task.Status == "pending" {
		task, err = client.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != "completed" {
		t.Fatalf("native parent did not continue: %+v", task)
	}
	if _, err = os.Stat(filepath.Join(workspace, "cancelled.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped write executed: %v", err)
	}
	var grants, pending int
	if err = client.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_background_permission_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("cancellation generated grants: %d %v", grants, err)
	}
	if err = client.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE state='pending'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending effects remain: %d %v", pending, err)
	}
	inner, err = client.TaskTreeNode(ctx, session.ID, inner.RootID, inner.ID)
	if err != nil || inner.Status != "cancelled" {
		t.Fatalf("inner state=%+v %v", inner, err)
	}
}
