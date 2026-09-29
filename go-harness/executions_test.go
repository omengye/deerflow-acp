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
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKExecutionCheckpointReopenApproveDenyCancel(t *testing.T) {
	for _, decision := range []harness.PermissionDecision{harness.AllowOnce, harness.RejectOnce, harness.PermissionCancelled} {
		t.Run(string(decision), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					// Whitespace is intentional: checkpoint and intent must preserve
					// the exact bytes whose digest was approved before interruption.
					args := `{ "path": "approved.txt", "content": "only once" }`
					argJSON, _ := json.Marshal(args)
					fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"write-once\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", argJSON)
				} else {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Finished\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			ctx := context.Background()
			limits := harness.BudgetLimits{MaxModelCalls: 2, MaxToolCalls: 1, MaxTokens: 10000, MaxOutputTokens: 256}
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, Budget: &limits}
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
			sess, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "write the file"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.PermissionCancelled, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || !result.Execution.Resumable {
				t.Fatalf("paused=%+v err=%v", result, err)
			}
			before := *result.Execution
			if len(before.WaitingInputs) != 1 || before.Attempt != 1 {
				t.Fatalf("waiting=%+v", before)
			}
			if _, err := os.Stat(filepath.Join(workspace, "approved.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("effect before approval: %v", err)
			}
			if _, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "do not replace input"}}, nil, nil); !errors.Is(err, harness.ErrExecutionWaitingInput) {
				t.Fatalf("waiting prompt=%v", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			state, err := c.Execution(ctx, sess.ID, "")
			if err != nil || state.RunID != before.RunID || state.Version != before.Version || state.Status != harness.ExecutionWaitingInput {
				t.Fatalf("restored=%+v err=%v", state, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("attachment replayed provider: %d", calls.Load())
			}
			if decision == harness.PermissionCancelled {
				state, err = c.CancelExecution(ctx, sess.ID, harness.CancelExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version})
				if err != nil || state.Status != harness.ExecutionCancelled || state.Resumable {
					t.Fatalf("cancel=%+v err=%v", state, err)
				}
			} else {
				approved := 0
				result, err = c.ResumeExecution(ctx, sess.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
					approved++
					if req.RunID != state.RunID || req.ToolCallID != "write-once" || string(req.Arguments) != `{ "path": "approved.txt", "content": "only once" }` {
						t.Errorf("approval changed: %+v", req)
					}
					return decision, nil
				})
				if err != nil || result.Execution == nil || result.Execution.RunID != state.RunID || result.Execution.InputID != state.InputID || result.Execution.Status != harness.ExecutionCompleted || result.Execution.Attempt != 2 || approved != 1 {
					t.Fatalf("resumed=%+v err=%v approvals=%d", result, err, approved)
				}
				if calls.Load() != 2 {
					t.Fatalf("model calls=%d", calls.Load())
				}
			}
			if _, err := c.ResumeExecution(ctx, sess.ID, harness.ResumeExecutionRequest{RunID: before.RunID, ExpectedVersion: before.Version}, nil, nil); !errors.Is(err, harness.ErrExecutionConflict) {
				t.Fatalf("repeat=%v", err)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "approved.txt"))
			if decision == harness.AllowOnce {
				if err != nil || string(data) != "only once" {
					t.Fatalf("file=%q err=%v", data, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unapproved file=%q err=%v", data, err)
			}
			receipts, err := c.ListToolReceipts(ctx, sess.ID)
			want := harness.ReceiptNotExecuted
			if decision == harness.AllowOnce {
				want = harness.ReceiptCompleted
			}
			if err != nil || len(receipts) != 1 || receipts[0].State != want {
				t.Fatalf("receipts=%+v err=%v", receipts, err)
			}
			snapshot, err := c.budgets.Snapshot(ctx, before.RunID)
			if err != nil || snapshot.ToolCalls != 1 || snapshot.ModelCalls != int(calls.Load()) || snapshot.HeldTokens != 0 {
				t.Fatalf("budget=%+v err=%v", snapshot, err)
			}
			page, err := c.HistoryPage(ctx, sess.ID, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			starts, executes, inputs := 0, 0, 0
			for _, event := range page.Events {
				switch event.Kind {
				case "tool_start":
					starts++
				case "tool_execute":
					executes++
				case "user_message":
					inputs++
				}
			}
			wantExecutions := 0
			if decision == harness.AllowOnce {
				wantExecutions = 1
			}
			if starts != 1 || executes != wantExecutions || inputs != 1 {
				t.Fatalf("start=%d execute=%d inputs=%d", starts, executes, inputs)
			}
		})
	}
}
