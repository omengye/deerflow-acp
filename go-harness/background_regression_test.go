package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKBackgroundAgentStreamingAndLaterTurnInspection(t *testing.T) {
	for _, scenario := range []string{"streaming-only", "task_status", "task_status_failed", "task_cancel", "task_wait", "task_status_missing"} {
		t.Run(scenario, func(t *testing.T) {
			var childCalls atomic.Int32
			var target atomic.Value
			target.Store("")
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct {
						Role, Content string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture", http.StatusBadRequest)
					return
				}
				lastUser, toolResult := "", ""
				for _, m := range req.Messages {
					if m.Role == "user" {
						lastUser, toolResult = m.Content, ""
					} else if m.Role == "tool" {
						toolResult = m.Content
					}
				}
				if strings.Contains(lastUser, "STREAM_CHILD") {
					childCalls.Add(1)
					if scenario == "task_status_failed" {
						http.Error(w, `{"error":{"message":"fixture model request rejected"}}`, http.StatusBadRequest)
						return
					}
					if scenario == "streaming-only" && !req.Stream {
						http.Error(w, `{"error":{"message":"Non-stream chat request is currently not supported"}}`, http.StatusBadRequest)
						return
					}
					skillToolResponse(w, req.Stream, "", "", "BG-CHILD-OK")
					return
				}
				if toolResult != "" {
					if lastUser == "INSPECT_PREVIOUS_TURN" {
						want := `"status":"completed"`
						if scenario == "task_wait" {
							want = "permission denied"
						} else if scenario == "task_status_failed" {
							want = `"status":"failed"`
						} else if scenario == "task_status_missing" {
							want = "not found"
						}
						if !strings.Contains(toolResult, want) {
							t.Errorf("inspection result=%s want %s", toolResult, want)
						}
					}
					skillToolResponse(w, req.Stream, "", "", "parent finished")
					return
				}
				if lastUser == "INSPECT_PREVIOUS_TURN" {
					name := scenario
					id := target.Load().(string)
					if scenario == "task_status_missing" {
						name, id = "task_status", "missing-task"
					} else if scenario == "task_status_failed" {
						name = "task_status"
					}
					args := fmt.Sprintf(`{"taskId":%q}`, id)
					if name == "task_wait" {
						args = fmt.Sprintf(`{"taskId":%q,"afterVersion":1}`, id)
					}
					skillToolResponse(w, req.Stream, name, args, "")
					return
				}
				skillToolResponse(w, req.Stream, "background_agent", `{"instruction":"STREAM_CHILD: reply BG-CHILD-OK without tools"}`, "")
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeOff})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			session, err := c.NewSession(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			first, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "START_CHILD"}}, nil, nil)
			if err != nil || first.Execution == nil || first.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			tasks, err := c.BackgroundTasks(ctx, session.ID, "", 10)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%+v err=%v", tasks, err)
			}
			task := tasks[0]
			for task.Status == "pending" || task.Status == "running" {
				task, err = c.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
				if err != nil {
					t.Fatal(err)
				}
			}
			wantStatus := "completed"
			if scenario == "task_status_failed" {
				wantStatus = "failed"
			}
			if task.Status != wantStatus || childCalls.Load() != 1 || wantStatus == "completed" && !strings.Contains(string(task.Result), "BG-CHILD-OK") {
				t.Fatalf("child calls=%d task=%+v", childCalls.Load(), task)
			}
			if scenario == "streaming-only" {
				return
			}
			target.Store(task.ID)
			second, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "INSPECT_PREVIOUS_TURN"}}, nil, nil)
			if err != nil || second.Execution == nil || second.Execution.Status != harness.ExecutionCompleted || second.Execution.RunID == first.Execution.RunID {
				t.Fatalf("later inspection=%+v err=%v", second, err)
			}
			receipts, err := c.ListToolReceipts(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, receipt := range receipts {
				if receipt.State == harness.ReceiptUncertain || receipt.State == harness.ReceiptStarted {
					t.Fatalf("inspection blocked session: %+v", receipt)
				}
			}
			var deliveries int
			if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_task_deliveries WHERE run_id=?`, second.Execution.RunID).Scan(&deliveries); err != nil || deliveries != 0 {
				t.Fatalf("later turn consumed prior root delivery: %d %v", deliveries, err)
			}
			if childCalls.Load() != 1 {
				t.Fatal("inspection replayed child execution")
			}
		})
	}
}
