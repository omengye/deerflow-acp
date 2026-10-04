package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

func TestSDKMixedRoleAncestryAndSingleSlotWait(t *testing.T) {
	for _, modes := range []string{"FBF", "BFB", "FFB", "BBF", "BFF", "FBB", "BBB"} {
		t.Run(modes, func(t *testing.T) { runMixedRoleTree(t, modes, false) })
	}
	for _, modes := range []string{"BFB", "BBF", "FBF", "FFB", "FBB", "BBB"} {
		t.Run(modes+"_restart", func(t *testing.T) { runMixedRoleTree(t, modes, true) })
	}
	t.Run("BBB_parallel_root_permission_restart", func(t *testing.T) { runMixedRoleTree(t, "BBB", true, true) })
}

func TestSDKMixedRoleAutomaticWakeup(t *testing.T) {
	for _, modes := range []string{"FBF", "BBB"} {
		t.Run(modes+"_restart", func(t *testing.T) { runMixedRoleTree(t, modes, true, false, true) })
	}
}

func runMixedRoleTree(t *testing.T, modes string, restart bool, options ...bool) {
	parallelRootPermission := len(options) > 0 && options[0]
	automatic := len(options) > 1 && options[1]
	var rootCalls atomic.Int32
	names := []string{"parent", "one", "two", "three"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                                     `json:"model"`
			Stream   bool                                       `json:"stream"`
			Messages []struct{ Role, Content string }           `json:"messages"`
			Tools    []struct{ Function struct{ Name string } } `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		depth := -1
		for i, name := range names {
			if name == request.Model {
				depth = i
			}
		}
		if depth < 0 {
			t.Errorf("unexpected model %s", request.Model)
			http.Error(w, "invalid", 400)
			return
		}
		if depth == 0 {
			rootCalls.Add(1)
		}
		hasTool := false
		var last harness.BackgroundTask
		for _, message := range request.Messages {
			if message.Role == "tool" {
				hasTool = true
				_ = json.Unmarshal([]byte(message.Content), &last)
			}
		}
		message := map[string]any{"role": "assistant", "content": "finished " + request.Model}
		finish := "stop"
		toolName, args := "", ""
		if depth < 3 && !hasTool {
			toolName, args = "task", fmt.Sprintf(`{"subagent_type":%q,"prompt":"isolated work","description":"mixed tree"}`, names[depth+1])
			if modes[depth] == 'B' {
				toolName, args = "background_agent", fmt.Sprintf(`{"role":%q,"instruction":"isolated work"}`, names[depth+1])
			}
		} else if depth < 3 && modes[depth] == 'B' && last.Status != "completed" {
			if last.ID == "" || last.Status == "failed" || last.Status == "canceled" {
				t.Errorf("%s did not get a live child: %+v", request.Model, last)
			}
			toolName, args = "task_wait", fmt.Sprintf(`{"taskId":%q,"afterVersion":%d}`, last.ID, last.Version)
		}
		if toolName != "" {
			available := false
			for _, tool := range request.Tools {
				available = available || tool.Function.Name == toolName
			}
			if !available {
				t.Errorf("%s lacks %s", request.Model, toolName)
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": toolName + "-" + request.Model, "type": "function", "function": map[string]string{"name": toolName, "arguments": args}}}
			if parallelRootPermission && depth == 0 && toolName == "task_wait" {
				message["tool_calls"] = append(message["tool_calls"].([]any), map[string]any{"index": 1, "id": "root-write", "type": "function", "function": map[string]string{"name": "write_file", "arguments": `{"path":"mixed-root.txt","content":"once"}`}})
			}
			finish = "tool_calls"
		}
		if restart && depth == 3 && !hasTool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "leaf-write", "type": "function", "function": map[string]string{"name": "write_file", "arguments": `{"path":"mixed-leaf.txt","content":"once"}`}}}
			finish = "tool_calls"
		}
		if depth == 3 {
			for _, tool := range request.Tools {
				if tool.Function.Name == "background_agent" || tool.Function.Name == "task" {
					t.Error("leaf can exceed mixed depth")
				}
			}
		}
		writeTreeModelResponse(w, request.Stream, message, finish)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	depth, concurrent, count := 3, 1, 3
	limits := harness.BudgetLimits{MaxModelCalls: 30, MaxToolCalls: 12, MaxTokens: 100000, MaxOutputTokens: 128}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeOff, Budget: &limits,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{MaxDepth: &depth, MaxConcurrent: &concurrent, MaxTasksPerRoot: &count, CustomAgents: map[string]harness.AgentConfig{}}}
	if restart {
		cfg.PermissionMode = harness.PermissionModeDangerous
	}
	for i, name := range names[1:] {
		cfg.ModelRoutes[name] = harness.ModelRoute{Provider: "openai", Model: name, APIKey: "fixture", BaseURL: provider.URL + "/v1"}
		role := harness.AgentConfig{Description: name, SystemPrompt: "Complete the work", Model: name}
		if i < 2 {
			role.DelegateTo = []string{names[i+2]}
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
	rootApprovals := 0
	result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(_ context.Context, permission harness.PermissionRequest) (harness.PermissionDecision, error) {
		if permission.ToolCallID == "root-write" {
			rootApprovals++
		}
		return harness.AllowOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parallelRootPermission {
		// One permission resume creates the background task, the next settles
		// the parallel root write. Neither resumes the still-waiting child.
		if rootApprovals != 1 || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || result.Execution.Resumable || len(result.Execution.WaitingInputs) != 0 || len(result.Execution.WaitingTasks) != 1 || result.Execution.Attempt != 3 {
			t.Fatalf("mixed root did not settle permission before waiting: %+v approvals=%d", result.Execution, rootApprovals)
		}
		before := rootCalls.Load()
		premature, resumeErr := client.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, nil)
		if resumeErr != nil || premature.Execution == nil || premature.Execution.Attempt != result.Execution.Attempt || rootCalls.Load() != before {
			t.Fatalf("premature resume changed execution/model spending: %+v %v", premature, resumeErr)
		}
	}
	var tasks []harness.BackgroundTask
	reopened := false
	updates := make(chan harness.RunResult, 2)
	updateErrors := make(chan error, 1)
	for {
		tasks, err = client.BackgroundTasks(ctx, session.ID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		complete := len(tasks) == strings.Count(modes, "B")
		if restart && !reopened && complete {
			paused := true
			for _, task := range tasks {
				paused = paused && task.Status == "waiting_input" && task.Interaction != nil
			}
			if paused {
				if err = client.Close(); err != nil {
					t.Fatal(err)
				}
				client = nil
				cfg.Subagents.CustomAgents = map[string]harness.AgentConfig{}
				client, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = client.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				reopened = true
				if automatic {
					go func() {
						updateErrors <- client.ServeSessionUpdates(ctx, session.ID, harness.SessionUpdates{Result: func(_ context.Context, result harness.RunResult, err error) error {
							if err == nil {
								updates <- result
							}
							return err
						}})
					}()
				}
			}
		}
		for _, task := range tasks {
			if task.Status == "failed" || task.Status == "canceled" || task.BlockedReason != "" {
				t.Fatalf("mixed task failed: %+v", task)
			}
			complete = complete && task.Status == "completed"
			if task.Status == "waiting_input" && task.Interaction != nil && task.Interaction.Resumable {
				leaf := false
				for _, input := range task.Interaction.WaitingInputs {
					leaf = leaf || input.ToolName == "write_file"
				}
				if leaf && restart && !reopened {
					continue
				}
				if _, err = client.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if complete {
			if automatic {
				select {
				case resumed := <-updates:
					if resumed.Execution == nil || resumed.Execution.Status != harness.ExecutionCompleted || resumed.Execution.RunID != result.Execution.RunID || resumed.Execution.InputID != result.Execution.InputID {
						t.Fatalf("automatic mixed resume: %+v", resumed)
					}
				case err := <-updateErrors:
					t.Fatalf("automatic updates stopped: %v", err)
				case <-ctx.Done():
					t.Fatal("automatic mixed resume stalled")
				}
				break
			}
			state, stateErr := client.Execution(ctx, session.ID, result.Execution.RunID)
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			if state.Status == harness.ExecutionWaitingInput {
				if !state.Resumable || len(state.WaitingInputs) != 0 || len(state.WaitingTasks) == 0 {
					t.Fatalf("finished descendants did not enable foreground resume: %+v", state)
				}
				resumed, resumeErr := client.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, nil)
				if resumeErr != nil || resumed.Execution == nil || resumed.Execution.Status != harness.ExecutionCompleted || resumed.Execution.RunID != result.Execution.RunID || resumed.Execution.InputID != result.Execution.InputID {
					t.Fatalf("mixed foreground resume: %+v %v", resumed, resumeErr)
				}
			} else if state.Status != harness.ExecutionCompleted {
				t.Fatalf("foreground did not finish: %+v", state)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("mixed tree stalled: %+v", tasks)
		case <-time.After(10 * time.Millisecond):
		}
	}
	rows, err := client.store.DB().QueryContext(ctx, `SELECT id,root_id,parent_id,depth,spec_json FROM harness_agent_invocations WHERE root_id=? ORDER BY depth`, result.Execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var invocations []agents.Invocation
	for rows.Next() {
		var in agents.Invocation
		var spec []byte
		if err = rows.Scan(&in.ID, &in.RootID, &in.ParentID, &in.Depth, &spec); err != nil {
			break
		}
		if err = json.Unmarshal(spec, &in.Spec); err != nil {
			break
		}
		invocations = append(invocations, in)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || len(invocations) != 3 {
		t.Fatalf("mixed role registrations: %+v %v", invocations, err)
	}
	for i, in := range invocations {
		parent := ""
		if i > 0 {
			parent = invocations[i-1].ID
		}
		if in.Depth != i+1 || in.ParentID != parent || in.Spec.Name != names[i+1] || in.RootID != result.Execution.RunID {
			t.Fatalf("wrong mixed ancestry: %+v", in)
		}
	}
	for _, task := range tasks {
		if task.Depth < 1 || task.Depth > 3 || modes[task.Depth-1] != 'B' || task.RootSessionID != session.ID {
			t.Fatalf("wrong tree projection: %+v", task)
		}
		wantParent := ""
		for i := task.Depth - 2; i >= 0; i-- {
			if modes[i] == 'B' {
				wantParent = strings.TrimPrefix(invocations[i].ID, "background/")
				break
			}
		}
		if task.ParentTaskID != wantParent {
			t.Fatalf("wrong nearest background parent: %+v", task)
		}
		wantRole := ""
		if task.Depth > 1 {
			wantRole = invocations[task.Depth-2].ID
		}
		if task.ParentInvocationID != wantRole {
			t.Fatalf("missing parent role in task: %+v", task)
		}
		var received int
		if err = client.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_task_deliveries WHERE task_id=? AND task_version=? AND recipient_role=?`, task.ID, task.Version, wantRole).Scan(&received); err != nil || received != 1 {
			t.Fatalf("mixed result did not reach its original parent role: task=%s role=%s count=%d err=%v", task.ID, wantRole, received, err)
		}
	}
	if restart {
		if !reopened {
			t.Fatal("mixed tree never restarted")
		}
		data, err := os.ReadFile(filepath.Join(workspace, "mixed-leaf.txt"))
		if err != nil || string(data) != "once" {
			t.Fatalf("leaf effect: %q %v", data, err)
		}
		var writes int
		wantWrites := 1
		if parallelRootPermission {
			wantWrites++
		}
		if err = client.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_tool_receipts WHERE state='completed' AND json_extract(receipt,'$.toolName')='write_file'`).Scan(&writes); err != nil || writes != wantWrites {
			t.Fatalf("leaf receipts=%d %v", writes, err)
		}
	}
}
