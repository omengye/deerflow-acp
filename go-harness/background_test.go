package deerflow

import (
	"context"
	"encoding/json"
	"errors"
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
)

func TestSDKNativeBackgroundChildApprovalAcrossRestart(t *testing.T) {
	for _, decision := range []harness.PermissionDecision{harness.AllowOnce, harness.RejectOnce, harness.PermissionCancelled, "inherited_allow_always"} {
		t.Run(string(decision), func(t *testing.T) {
			var parentCalls, childCalls atomic.Int32
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
					http.Error(w, "bad fixture request", 400)
					return
				}
				isChild, hasTool := false, false
				for _, m := range request.Messages {
					if m.Role == "user" && strings.Contains(string(m.Content), "CHILD_INSTRUCTION") {
						isChild = true
					}
					if m.Role == "tool" {
						hasTool = true
					}
				}
				if isChild {
					childCalls.Add(1)
				} else {
					parentCalls.Add(1)
				}
				if !request.Stream {
					message := map[string]any{"role": "assistant", "content": "Work finished"}
					finish := "stop"
					if !hasTool {
						name, id, args := "background_agent", "delegate-once", `{ "instruction": "CHILD_INSTRUCTION: write child.txt", "description": "child work" }`
						if isChild {
							name, id, args = "write_file", "child-write-once", `{ "path": "child.txt", "content": "durable child" }`
						}
						message["content"] = nil
						message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
						finish = "tool_calls"
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if !hasTool {
					name, id, args := "background_agent", "delegate-once", `{ "instruction": "CHILD_INSTRUCTION: write child.txt", "description": "child work" }`
					if isChild {
						name, id, args = "write_file", "child-write-once", `{ "path": "child.txt", "content": "durable child" }`
					}
					raw, _ := json.Marshal(args)
					fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, raw)
				} else {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Work finished\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			limits := harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 2, MaxTokens: 100000, MaxOutputTokens: 256}
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, Budget: &limits}
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					if err := c.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			workspace := t.TempDir()
			sess, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if decision == "inherited_allow_always" {
				if _, err = c.SetConfigOption(ctx, sess.ID, "approval", harness.ApprovalAllowAlways); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "Delegate the child work"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
				if decision == "inherited_allow_always" {
					t.Error("inherited policy unnecessarily requested human permission")
				}
				if p.ToolName != "background_agent" {
					t.Errorf("parent approved child effect: %s", p.ToolName)
				}
				return harness.AllowOnce, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("parent result=%+v err=%v", result, err)
			}
			tasks, err := c.BackgroundTasks(ctx, sess.ID, "", 10)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%+v err=%v", tasks, err)
			}
			task := tasks[0]
			for task.Status == "pending" || task.Status == "running" {
				task, err = c.WaitBackgroundTask(ctx, sess.ID, task.ID, task.Version)
				if err != nil {
					t.Fatal(err)
				}
			}
			if decision == "inherited_allow_always" {
				if task.Status != "completed" || task.Interaction != nil || task.Attempt != 1 || task.BlockedReason != "" {
					t.Fatalf("policy did not complete child in one attempt: %+v", task)
				}
				data, readErr := os.ReadFile(filepath.Join(workspace, "child.txt"))
				if readErr != nil || string(data) != "durable child" {
					t.Fatalf("inherited policy effect=%q err=%v", data, readErr)
				}
				budget, snapshotErr := c.budgets.Snapshot(ctx, result.Execution.RunID)
				if snapshotErr != nil || budget.ToolCalls != 2 || budget.ModelCalls != 4 || budget.HeldTokens != 0 || budget.BlockedReason != "" {
					t.Fatalf("inherited policy shared budget=%+v err=%v", budget, snapshotErr)
				}
				return
			}
			if task.Status != "waiting_input" || task.Interaction == nil || !task.Interaction.Resumable || len(task.Interaction.WaitingInputs) != 1 {
				t.Fatalf("child did not durably pause: %+v", task)
			}
			preview, previewErr := c.BackgroundPermission(ctx, sess.ID, harness.BackgroundPermissionQuery{TaskID: task.ID, InteractionID: task.Interaction.ID, TaskVersion: task.Version, IntentID: task.Interaction.WaitingInputs[0].ID})
			if previewErr != nil || preview.ToolName != "write_file" || string(preview.Arguments) != `{ "path": "child.txt", "content": "durable child" }` {
				t.Fatalf("saved child approval preview=%+v err=%v", preview, previewErr)
			}
			if _, err := os.Stat(filepath.Join(workspace, "child.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child executed before approval: %v", err)
			}
			if parentCalls.Load() != 2 || childCalls.Load() != 1 {
				t.Fatalf("provider calls parent=%d child=%d", parentCalls.Load(), childCalls.Load())
			}
			if _, err := c.LoadSession(ctx, task.ChildSessionID, workspace, false, nil); err == nil {
				t.Fatal("foreground attached to task child")
			}
			listed, err := c.service.Store.List(ctx, "", "", 100)
			if err != nil || len(listed) != 1 || listed[0].ID != sess.ID {
				t.Fatalf("child leaked through normal list: %+v %v", listed, err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			reopened, err := c.BackgroundTask(ctx, sess.ID, task.ID)
			if err != nil || reopened.Interaction == nil || !reopened.Interaction.Resumable || reopened.Version != task.Version || reopened.Interaction.ID != task.Interaction.ID {
				t.Fatalf("reopened=%+v err=%v", reopened, err)
			}
			if childCalls.Load() != 1 {
				t.Fatal("startup replayed waiting task")
			}
			approval := harness.TaskApproval{ID: reopened.Interaction.ID, TaskVersion: reopened.Version, Decision: decision}
			if decision == harness.PermissionCancelled {
				task, err = c.CancelBackgroundTask(ctx, sess.ID, task.ID)
			} else {
				task, err = c.ApproveBackgroundTask(ctx, sess.ID, task.ID, approval)
			}
			if err != nil {
				t.Fatal(err)
			}
			for task.Status == "pending" || task.Status == "running" {
				task, err = c.WaitBackgroundTask(ctx, sess.ID, task.ID, task.Version)
				if err != nil {
					t.Fatal(err)
				}
			}
			wantStatus := "completed"
			if decision == harness.PermissionCancelled {
				wantStatus = "canceled"
			}
			if task.Status != wantStatus || task.BlockedReason != "" {
				t.Fatalf("finished child=%+v", task)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
			if decision == harness.AllowOnce {
				if err != nil || string(data) != "durable child" {
					t.Fatalf("child file=%q err=%v", data, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unapproved child effect=%q err=%v", data, err)
			}
			budget, err := c.budgets.Snapshot(ctx, result.Execution.RunID)
			if err != nil || budget.ToolCalls != 2 || budget.ModelCalls != int(parentCalls.Load()+childCalls.Load()) || budget.HeldTokens != 0 || budget.BlockedReason != "" {
				t.Fatalf("shared budget=%+v err=%v", budget, err)
			}
			if decision != harness.PermissionCancelled && childCalls.Load() != 2 {
				t.Fatalf("child repeated model=%d", childCalls.Load())
			}
			if _, err := c.ApproveBackgroundTask(ctx, sess.ID, task.ID, approval); err == nil {
				t.Fatal("stale task approval executed twice")
			}
		})
	}
}
