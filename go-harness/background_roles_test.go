package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKForegroundAndBackgroundRolesShareConcurrency(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var childCalls, active, peak atomic.Int32
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model    string                  `json:"model"`
					Stream   bool                    `json:"stream"`
					Messages []struct{ Role string } `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				message := map[string]any{"role": "assistant", "content": "done"}
				finish := "stop"
				if request.Model == "child" {
					childCalls.Add(1)
					n := active.Add(1)
					defer active.Add(-1)
					for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
					}
					entered <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				} else {
					hasTool := false
					for _, m := range request.Messages {
						hasTool = hasTool || m.Role == "tool"
					}
					if !hasTool {
						message["content"] = nil
						message["tool_calls"] = []any{
							map[string]any{"index": 0, "id": "front", "type": "function", "function": map[string]string{"name": "task", "arguments": `{"subagent_type":"worker","prompt":"foreground work","description":"foreground"}`}},
							map[string]any{"index": 1, "id": "back", "type": "function", "function": map[string]string{"name": "background_agent", "arguments": `{"role":"worker","instruction":"background work","description":"background"}`}},
						}
						finish = "tool_calls"
					}
				}
				if request.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					body, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
				}
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			limits := harness.BudgetLimits{MaxModelCalls: 6, MaxToolCalls: 2, MaxTokens: 100000, MaxOutputTokens: 128}
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 2, PermissionMode: harness.PermissionModeOff, Budget: &limits,
				ModelRoutes: map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", BaseURL: provider.URL + "/v1", APIKey: "fixture"}},
				Subagents:   &harness.SubagentsConfig{MaxConcurrent: &limit, CustomAgents: map[string]harness.AgentConfig{"worker": {Description: "work", SystemPrompt: "Complete the delegated task", Model: "child", Tools: []string{}}}},
			}
			client, err := Open(ctx, cfg)
			if err != nil {
				unblock()
				t.Fatal(err)
			}
			defer client.Close()
			defer unblock()
			session, err := client.NewSession(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result harness.RunResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delegate both tasks"}}, nil, nil)
				done <- outcome{result, err}
			}()
			for i := 0; i < limit; i++ {
				select {
				case <-entered:
				case result := <-done:
					t.Fatalf("parent returned early: %v", result.err)
				case <-ctx.Done():
					t.Fatal("children failed to enter provider")
				}
			}
			select {
			case <-entered:
				t.Error("mixed tasks exceeded concurrency")
			case <-time.After(75 * time.Millisecond):
			}
			unblock()
			var completed outcome
			select {
			case completed = <-done:
				if completed.err != nil {
					t.Fatal(completed.err)
				}
			case <-ctx.Done():
				t.Fatal("parent did not complete")
			}
			tasks, err := client.BackgroundTasks(ctx, session.ID, "", 10)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%v err=%v", tasks, err)
			}
			task := tasks[0]
			for task.Status == "pending" || task.Status == "running" {
				task, err = client.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
				if err != nil {
					t.Fatal(err)
				}
			}
			if task.Status != "completed" || childCalls.Load() != 2 || peak.Load() != int32(limit) {
				t.Fatalf("task=%+v calls=%d peak=%d", task, childCalls.Load(), peak.Load())
			}
			var count int
			if err = client.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_agent_invocations WHERE root_id=?`, completed.result.Execution.RunID).Scan(&count); err != nil || count != 2 {
				t.Fatalf("root registrations=%d err=%v", count, err)
			}
			assertSDKMixedTaskTree(t, ctx, client, session.ID, completed.result.Execution.RunID, task.ID)
		})
	}
}

func assertSDKMixedTaskTree(t *testing.T, ctx context.Context, client *Client, sessionID, rootID, backgroundID string) {
	t.Helper()
	query := harness.TaskTreeQuery{RootID: rootID, Limit: 1}
	nodes := map[string]harness.TaskTreeNode{}
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatal("task tree cursor did not finish")
		}
		page, err := client.TaskTree(ctx, sessionID, query)
		if err != nil {
			t.Fatal(err)
		}
		if page.SessionID != sessionID || page.Consistency != "live_keyset" || len(page.Nodes) != 1 {
			t.Fatalf("tree page: %+v", page)
		}
		node := page.Nodes[0]
		if _, exists := nodes[node.ID]; exists {
			t.Fatalf("duplicate logical role: %s", node.ID)
		}
		nodes[node.ID] = node
		if node.RootID != rootID || node.RootSessionID != sessionID {
			t.Fatalf("tree scope: %+v", node)
		}
		if node.Status != "completed" {
			t.Fatalf("finished actual model role not projected: %+v", node)
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("missing live cursor")
		}
		query.Cursor = page.NextCursor
	}
	if len(nodes) != 3 {
		t.Fatalf("want root and two logical roles: %+v", nodes)
	}
	var front, back int
	for _, node := range nodes {
		if node.Mode == "root" {
			if node.ID != "root/"+rootID || node.Usage == nil || node.Usage.Scope != "root_total" {
				t.Fatalf("root usage/identity: %+v", node)
			}
			continue
		}
		if node.ParentID != "root/"+rootID || node.Role != "worker" || node.Model != "child" {
			t.Fatalf("mixed role parent/model: %+v", node)
		}
		if node.Result == nil || node.Result.Text != "done" {
			t.Fatalf("lost final public role answer: %+v", node)
		}
		if node.Mode == "foreground" {
			front++
		} else if node.Mode == "background" {
			back++
			if node.TaskID != backgroundID {
				t.Fatalf("lost native execution reference: %+v", node)
			}
		}
		got, err := client.TaskTreeNode(ctx, sessionID, rootID, node.ID)
		if err != nil || got.ID != node.ID || got.Version != node.Version {
			t.Fatalf("tree get mismatch: %+v %v", got, err)
		}
	}
	if front != 1 || back != 1 {
		t.Fatalf("mixed role modes: %+v", nodes)
	}
	other, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for id := range nodes {
		if _, err := client.TaskTreeNode(ctx, other.ID, rootID, id); err == nil {
			t.Fatal("foreign session could read task tree")
		}
	}
}
