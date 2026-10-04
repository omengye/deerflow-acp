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

func TestSDKRoleCannotUseRootTaskAuthorityForSibling(t *testing.T) {
	for _, forbidden := range []string{"task_status", "task_wait", "task_cancel"} {
		t.Run(forbidden, func(t *testing.T) {
			var mu sync.Mutex
			var sibling harness.BackgroundTask
			sawDenial := false
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string                           `json:"model"`
					Stream   bool                             `json:"stream"`
					Messages []struct{ Role, Content string } `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "bad", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				results := []string{}
				for _, msg := range req.Messages {
					if msg.Role == "tool" {
						results = append(results, msg.Content)
					}
				}
				message := map[string]any{"role": "assistant", "content": "kept working"}
				finish := "stop"
				name, args := "", ""
				if req.Model == "parent" {
					if len(results) == 0 {
						name, args = "background_agent", `{"role":"other","instruction":"private sibling work"}`
					} else if len(results) == 1 {
						if err := json.Unmarshal([]byte(results[0]), &sibling); err != nil || sibling.ID == "" {
							t.Errorf("bad sibling: %s %v", results[0], err)
						}
						name, args = "task", `{"subagent_type":"scoped","prompt":"try task operation","description":"scoped role"}`
					}
				} else if req.Model == "scoped" {
					if len(results) == 0 {
						name, args = forbidden, fmt.Sprintf(`{"taskId":%q}`, sibling.ID)
						if forbidden == "task_wait" {
							args = fmt.Sprintf(`{"taskId":%q,"afterVersion":%d}`, sibling.ID, sibling.Version)
						}
					} else {
						sawDenial = strings.Contains(results[0], "Tool failed:") && !strings.Contains(results[0], "PRIVATE-SIBLING-RESULT")
						if !sawDenial {
							t.Errorf("sibling access was not safely denied: %q", results[0])
						}
					}
				} else {
					message["content"] = "PRIVATE-SIBLING-RESULT"
				}
				if name != "" {
					message["content"] = nil
					message["tool_calls"] = []any{map[string]any{"index": 0, "id": name + "-" + req.Model, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
					finish = "tool_calls"
				}
				writeTreeModelResponse(w, req.Stream, message, finish)
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			depth, concurrency, tasks := 3, 1, 2
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeOff, BackgroundWorkers: 1,
				ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{MaxDepth: &depth, MaxConcurrent: &concurrency, MaxTasksPerRoot: &tasks, CustomAgents: map[string]harness.AgentConfig{}}}
			for _, role := range []string{"other", "scoped"} {
				cfg.ModelRoutes[role] = harness.ModelRoute{Provider: "openai", Model: role, APIKey: "fixture", BaseURL: provider.URL + "/v1"}
				cfg.Subagents.CustomAgents[role] = harness.AgentConfig{Description: role, SystemPrompt: "Complete work", Model: role}
			}
			client, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "test scoped role"}}, nil, nil); err != nil {
				t.Fatal("denied task operation failed the conversation", err)
			}
			mu.Lock()
			denied, id := sawDenial, sibling.ID
			mu.Unlock()
			if !denied {
				t.Fatal("denial was not returned to the role")
			}
			listed, err := client.BackgroundTasks(ctx, session.ID, "", 100)
			if err != nil || len(listed) != 1 || listed[0].ID != id || listed[0].Status == "canceled" || listed[0].Status == "failed" {
				t.Fatalf("sibling was changed: %+v %v", listed, err)
			}
			var uncertain int
			if err = client.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE state IN ('uncertain','started')`).Scan(&uncertain); err != nil || uncertain != 0 {
				t.Fatalf("denial left uncertain effects: %d %v", uncertain, err)
			}
		})
	}
}
