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

func TestSDKBackgroundDrainReopenExplicitResumeDoesNotRepeatWrite(t *testing.T) {
	var parentCalls, childCalls atomic.Int32
	secondChildStarted, secondChildStopped := make(chan struct{}), make(chan struct{})
	thirdChildStarted, releaseBlocked := make(chan struct{}), make(chan struct{})
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
			http.Error(w, "invalid fixture request", 400)
			return
		}
		child, hasTool := false, false
		for _, message := range request.Messages {
			child = child || message.Role == "user" && strings.Contains(string(message.Content), "DRAIN_CHILD_INSTRUCTION")
			hasTool = hasTool || message.Role == "tool"
		}
		if child {
			call := childCalls.Add(1)
			if call == 2 {
				if !hasTool {
					t.Error("second child model lacks committed tool result")
				}
				close(secondChildStarted)
				select {
				case <-r.Context().Done():
				case <-releaseBlocked:
				}
				close(secondChildStopped)
				return
			}
			if call == 3 {
				close(thirdChildStarted)
			}
		} else {
			parentCalls.Add(1)
		}
		name, id, args := "background_agent", "drain-delegate", `{ "instruction": "DRAIN_CHILD_INSTRUCTION: write child.txt", "description": "drain test" }`
		if child {
			name, id, args = "write_file", "drain-child-write", `{ "path": "child.txt", "content": "original child write" }`
		}
		message := map[string]any{"role": "assistant", "content": "Work finished"}
		finish := "stop"
		if !hasTool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if !hasTool {
			raw, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, raw)
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Work finished\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	t.Cleanup(cancel)
	limits := harness.BudgetLimits{MaxModelCalls: 10, MaxToolCalls: 2, MaxTokens: 100000, MaxOutputTokens: 256}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, Budget: &limits}
	client, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if client != nil {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// If an assertion fails, unblock the fixture before joining the host.
	t.Cleanup(func() { close(releaseBlocked) })
	workspace := t.TempDir()
	session, err := client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delegate the work"}}, nil, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
		if req.ToolName != "background_agent" {
			return "", errors.New("parent was asked to approve a child tool")
		}
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("parent result=%+v err=%v", result, err)
	}
	tasks, err := client.BackgroundTasks(ctx, session.ID, "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	task := tasks[0]
	for task.Status == "pending" || task.Status == "running" {
		task, err = client.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != "waiting_input" || task.Interaction == nil || !task.Interaction.Resumable {
		t.Fatalf("child did not wait for write approval: %+v", task)
	}
	if _, err = client.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondChildStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	childFile := filepath.Join(workspace, "child.txt")
	if data, err := os.ReadFile(childFile); err != nil || string(data) != "original child write" {
		t.Fatalf("child effect not committed before drain: %q %v", data, err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("drain close: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("Close did not join draining child: %v", ctx.Err())
	}
	client = nil
	select {
	case <-secondChildStopped:
	case <-ctx.Done():
		t.Fatal("drain did not cancel the in-flight provider request")
	}
	client, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	task, err = client.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil || task.Status != "suspended" || task.BlockedReason != "" || task.Attempt != 2 {
		t.Fatalf("reopened drain task=%+v err=%v", task, err)
	}
	version := task.Version
	// Observe multiple dispatcher scans. Reopening/loading must preserve a
	// suspended boundary until the owner explicitly releases its exact version.
	select {
	case <-thirdChildStarted:
		t.Fatal("reopen automatically replayed suspended task")
	case <-time.After(750 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	task, err = client.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil || task.Status != "suspended" || task.Version != version || childCalls.Load() != 2 {
		t.Fatalf("suspension changed without owner: %+v calls=%d err=%v", task, childCalls.Load(), err)
	}
	if _, err = client.ResumeBackgroundTask(ctx, session.ID, task.ID, version-1); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale suspended version released: %v", err)
	}
	// Any repeated write_file would overwrite this marker. This verifies the
	// actual filesystem effect as well as the retained single logical receipt.
	const marker = "prior write verified; preserve on resume"
	if err := os.WriteFile(childFile, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	task, err = client.ResumeBackgroundTask(ctx, session.ID, task.ID, version)
	if err != nil {
		t.Fatal(err)
	}
	for task.Status == "pending" || task.Status == "running" {
		task, err = client.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != "completed" || task.Attempt != 3 || task.BlockedReason != "" {
		t.Fatalf("explicitly resumed task=%+v", task)
	}
	if data, err := os.ReadFile(childFile); err != nil || string(data) != marker {
		t.Fatalf("completed tool effect repeated: %q %v", data, err)
	}
	receipts, err := client.service.Store.ListToolReceipts(ctx, task.ChildSessionID)
	if err != nil || len(receipts) != 1 || receipts[0].ToolName != "write_file" || receipts[0].State != harness.ReceiptCompleted || receipts[0].Version != 3 {
		t.Fatalf("resumed receipts=%+v err=%v", receipts, err)
	}
	snapshot, err := client.budgets.Snapshot(ctx, result.Execution.RunID)
	if err != nil || snapshot.ToolCalls != 2 || snapshot.HeldTokens != 0 || snapshot.BlockedReason != "" || parentCalls.Load() != 2 || childCalls.Load() != 3 {
		t.Fatalf("budget=%+v parentCalls=%d childCalls=%d err=%v", snapshot, parentCalls.Load(), childCalls.Load(), err)
	}
	if _, err := client.ResumeBackgroundTask(ctx, session.ID, task.ID, version); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("completed task reused drained version: %v", err)
	}
}
