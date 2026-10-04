package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// One real model graph covers native background -> foreground -> background,
// a restarted logical-role continuation and a paused/resumed tool admission.
func TestSDKTaskTreeInvocationUsageAcrossBackgroundContinuation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var mu sync.Mutex
	providerCalls := map[string]int64{}
	inputs := map[string]int64{"root": 10, "host": 20, "parent": 30, "leaf": 40}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		mu.Lock()
		providerCalls[req.Model]++
		mu.Unlock()
		hasTool, note, wrote := false, false, false
		for _, m := range req.Messages {
			hasTool = hasTool || m.Role == "tool"
			note = note || m.Role == "user" && strings.Contains(m.Content, "Background task notification.")
			wrote = wrote || m.Role == "tool" && strings.Contains(m.Content, "usage-parent.txt")
		}
		name, args := "", ""
		switch req.Model {
		case "root":
			if !hasTool {
				name, args = "background_agent", `{"role":"host","instruction":"host work"}`
			}
		case "host":
			if !hasTool {
				name, args = "task", `{"subagent_type":"parent","prompt":"parent work","description":"parent"}`
			}
		case "parent":
			if note && !wrote {
				name, args = "write_file", `{"path":"usage-parent.txt","content":"once"}`
			} else if !note && !hasTool {
				name, args = "background_agent", `{"role":"leaf","instruction":"leaf work"}`
			}
		}
		message := map[string]any{"role": "assistant", "content": "finished " + req.Model}
		finish := "stop"
		if name != "" {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": name + "-" + req.Model, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		usage := map[string]int64{"prompt_tokens": inputs[req.Model], "completion_tokens": 1, "total_tokens": inputs[req.Model] + 1}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			usageData, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{}, "usage": usage})
			fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", data, usageData)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
		}
	}))
	defer provider.Close()
	depth, count, concurrent := 3, 3, 1
	automatic := false
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "root", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeDangerous,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{AutoContinue: &automatic, MaxDepth: &depth, MaxTasksPerRoot: &count, MaxConcurrent: &concurrent, CustomAgents: map[string]harness.AgentConfig{
			"host":   {Description: "host", SystemPrompt: "Host", Model: "host", DelegateTo: []string{"parent"}},
			"parent": {Description: "parent", SystemPrompt: "Parent", Model: "parent", DelegateTo: []string{"leaf"}},
			"leaf":   {Description: "leaf", SystemPrompt: "Leaf", Model: "leaf", Tools: []string{}},
		}}}
	for _, name := range []string{"host", "parent", "leaf"} {
		cfg.ModelRoutes[name] = harness.ModelRoute{Provider: "openai", Model: name, BaseURL: provider.URL + "/v1", APIKey: "fixture"}
	}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	workspace := t.TempDir()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || initial.Execution == nil {
		t.Fatalf("root=%+v %v", initial, err)
	}
	waitTasks := func(wantID string, wantCount int) {
		for {
			tasks, e := c.BackgroundTasks(ctx, session.ID, "", 100)
			if e != nil {
				t.Fatal(e)
			}
			complete := len(tasks) >= wantCount
			for _, task := range tasks {
				if task.Status == "failed" || task.Status == "canceled" {
					t.Fatalf("task failed: %+v", task)
				}
				if task.Status == "waiting_input" && task.Interaction != nil {
					if _, e = c.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); e != nil {
						t.Fatal(e)
					}
				}
				complete = complete && task.Status == "completed"
				if wantID != "" && task.ID == wantID && task.Status == "completed" {
					return
				}
			}
			if wantID == "" && complete {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("tasks did not finish: %+v", tasks)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitTasks("", 2)
	readTree := func() map[string]harness.TaskTreeNode {
		page, e := c.TaskTree(ctx, session.ID, harness.TaskTreeQuery{RootID: initial.Execution.RunID, Limit: 100})
		if e != nil {
			t.Fatal(e)
		}
		nodes := map[string]harness.TaskTreeNode{}
		for _, node := range page.Nodes {
			nodes[node.Role] = node
		}
		return nodes
	}
	before := readTree()
	parentID := before["parent"].ID
	if before["parent"].Mode != "foreground" || before["parent"].Usage == nil || before["host"].Usage == nil || before["leaf"].Usage == nil {
		t.Fatalf("initial attribution missing: %+v", before)
	}
	var note string
	for note == "" {
		err = c.store.DB().QueryRowContext(ctx, `SELECT notification_id FROM harness_background_inbox WHERE parent_session_id<>? AND json_extract(CAST(payload AS TEXT),'$.Kind')='completed' ORDER BY seq LIMIT 1`, session.ID).Scan(&note)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	continued, err := c.ProcessBackgroundNotification(ctx, session.ID, note, nil, nil)
	if err != nil || continued.Background == nil {
		t.Fatalf("continuation=%+v %v", continued, err)
	}
	waitTasks(continued.Background.ID, 3)
	after := readTree()
	if after["parent"].ID != parentID || after["parent"].Usage == nil || after["parent"].Usage.ModelCalls <= before["parent"].Usage.ModelCalls {
		t.Fatalf("continuation lost logical owner: %+v", after["parent"])
	}
	mu.Lock()
	calls := map[string]int64{}
	for name, value := range providerCalls {
		calls[name] = value
	}
	mu.Unlock()
	var childModels, childTools, childTokens, totalModels, totalTokens int64
	for name, value := range calls {
		totalModels += value
		totalTokens += value * (inputs[name] + 1)
	}
	for _, name := range []string{"host", "parent", "leaf"} {
		u := after[name].Usage
		if u == nil || u.Scope != "invocation" || u.ModelCalls != calls[name] || u.InputTokens != calls[name]*inputs[name] || u.OutputTokens != calls[name] || u.TotalTokens != calls[name]*(inputs[name]+1) || u.Estimated || u.HeldTokens != 0 {
			t.Fatalf("%s actual provider usage mismatch: usage=%+v calls=%d", name, u, calls[name])
		}
		childModels += u.ModelCalls
		childTools += u.ToolCalls
		childTokens += u.TotalTokens
	}
	root := after["lead"].Usage
	if root == nil || root.Scope != "root_total" || root.ModelCalls != totalModels || root.TotalTokens != totalTokens || root.ModelCalls-childModels != calls["root"] || root.TotalTokens-childTokens != calls["root"]*(inputs["root"]+1) {
		t.Fatalf("root double counted children: %+v actual models=%d tokens=%d", root, totalModels, totalTokens)
	}
	if after["parent"].Usage.ToolCalls != 2 || root.ToolCalls-childTools != 1 {
		t.Fatalf("resumed tool counted twice: parent=%+v root=%+v childTools=%d", after["parent"].Usage, root, childTools)
	}
	if again := readTree(); *again["parent"].Usage != *after["parent"].Usage || *again["lead"].Usage != *root {
		t.Fatal("read-only usage query mutated accounting")
	}
	// Old roots with any unattributed admission must not expose partial role
	// totals as complete. The authoritative root total remains unchanged.
	if _, err = c.store.DB().ExecContext(ctx, `DELETE FROM budget_operation_owners WHERE operation_id=(SELECT operation_id FROM budget_operation_owners WHERE root_id=? AND invocation_id='' LIMIT 1)`, initial.Execution.RunID); err != nil {
		t.Fatal(err)
	}
	unknown := readTree()
	if unknown["parent"].Usage != nil || unknown["host"].Usage != nil || unknown["leaf"].Usage != nil || *unknown["lead"].Usage != *root {
		t.Fatalf("legacy attribution invented totals: %+v", unknown)
	}
}
