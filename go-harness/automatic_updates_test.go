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
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Both SDK and ACP use the real Eino runner: the child waits for an explicit
// approval; the parent waits durably, then asks for a different write approval.
func newAutomaticUpdatesFixture(t *testing.T) *continuationFixture {
	return newAutomaticUpdatesFixtureWithPolicy(t, false)
}

func newAutomaticUpdatesFixtureWithPolicy(t *testing.T, auto bool) *continuationFixture {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string
				Content json.RawMessage
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "fixture", 400)
			return
		}
		toolCount := 0
		var last harness.BackgroundTask
		for _, m := range request.Messages {
			if m.Role != "tool" {
				continue
			}
			toolCount++
			var data string
			if json.Unmarshal(m.Content, &data) == nil {
				_ = json.Unmarshal([]byte(data), &last)
			}
		}
		name, args, callID := "", "", ""
		if request.Model == "child" {
			if toolCount == 0 {
				name, callID, args = "write_file", "auto-child-write", `{"path":"auto-child.txt","content":"child once"}`
			}
		} else {
			switch toolCount {
			case 0:
				name, callID, args = "background_agent", "auto-child", `{"role":"worker","instruction":"child work"}`
			case 1:
				name, callID, args = "task_wait", "auto-wait", fmt.Sprintf(`{"taskId":%q,"afterVersion":%d}`, last.ID, last.Version)
			case 2:
				name, callID, args = "write_file", "auto-root-write", `{"path":"auto-root.txt","content":"root once"}`
			}
		}
		message := map[string]any{"role": "assistant", "content": "completed " + request.Model}
		finish := "stop"
		if name != "" {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": callID, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		writeTreeModelResponse(w, request.Stream, message, finish)
	}))
	t.Cleanup(provider.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	limits := harness.BudgetLimits{MaxModelCalls: 12, MaxToolCalls: 8, MaxTokens: 100000, MaxOutputTokens: 128}
	concurrent := 1
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "parent", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeDangerous, Budget: &limits,
		ModelRoutes: map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", APIKey: "fixture", BaseURL: provider.URL + "/v1"}},
		Subagents:   &harness.SubagentsConfig{MaxConcurrent: &concurrent, AutoContinue: &auto, CustomAgents: map[string]harness.AgentConfig{"worker": {Description: "work", SystemPrompt: "Complete child work", Model: "child"}}}}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	sess, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &continuationFixture{c: c, cfg: cfg, ctx: ctx, sess: sess}
}

func waitingAutomaticChild(t *testing.T, f *continuationFixture) harness.BackgroundTask {
	t.Helper()
	for {
		tasks, err := f.c.BackgroundTasks(f.ctx, f.sess.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) == 1 {
			task := tasks[0]
			if task.Status == "failed" || task.BlockedReason != "" {
				t.Fatalf("child failed: %+v", task)
			}
			if task.Status == "waiting_input" && task.Interaction != nil && task.Interaction.Resumable {
				return task
			}
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("child never requested approval")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSDKAutomaticUpdatesFreshPermissionAndHandlerLifetime(t *testing.T) {
	for _, mode := range []string{"no_handler", "live_handler", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newAutomaticUpdatesFixture(t)
			var originalPermissions, livePermissions atomic.Int32
			before, err := f.c.Run(f.ctx, f.sess.ID, []harness.Content{{Type: "text", Text: "delegate then write"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				originalPermissions.Add(1)
				return harness.AllowOnce, nil
			})
			if err != nil || before.Execution == nil || len(before.Execution.WaitingTasks) != 1 {
				t.Fatalf("root did not wait: %+v %v", before, err)
			}
			permissionsBefore := originalPermissions.Load()
			child := waitingAutomaticChild(t, f)
			results := make(chan harness.RunResult, 2)
			done := make(chan error, 1)
			handlers := harness.SessionUpdates{Result: func(_ context.Context, result harness.RunResult, err error) error {
				if err == nil {
					results <- result
				}
				return err
			}}
			if mode != "no_handler" {
				handlers.Permission = func(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
					livePermissions.Add(1)
					if p.ToolCallID != "auto-root-write" {
						return harness.PermissionCancelled, fmt.Errorf("unexpected automatic approval %s", p.ToolCallID)
					}
					if mode == "cancel" {
						if err := f.c.Cancel(f.sess.ID); err != nil {
							return harness.PermissionCancelled, err
						}
						<-ctx.Done()
						return harness.PermissionCancelled, ctx.Err()
					}
					return harness.AllowOnce, nil
				}
			}
			go func() { done <- f.c.ServeSessionUpdates(f.ctx, f.sess.ID, handlers) }()
			if _, err = f.c.ApproveBackgroundTask(f.ctx, f.sess.ID, child.ID, harness.TaskApproval{ID: child.Interaction.ID, TaskVersion: child.Version, Decision: harness.AllowOnce}); err != nil {
				t.Fatal(err)
			}
			var resumed harness.RunResult
			select {
			case resumed = <-results:
			case err = <-done:
				t.Fatalf("updates stopped: %v", err)
			case <-f.ctx.Done():
				t.Fatal("automatic result missing")
			}
			want := harness.ExecutionCompleted
			if mode == "no_handler" {
				want = harness.ExecutionWaitingInput
			}
			if mode == "cancel" {
				want = harness.ExecutionCancelled
			}
			if resumed.Execution == nil || resumed.Execution.Status != want || resumed.Execution.RunID != before.Execution.RunID || resumed.Execution.InputID != before.Execution.InputID || originalPermissions.Load() != permissionsBefore {
				t.Fatalf("invalid automatic result/old callback reuse: %+v", resumed)
			}
			if mode == "no_handler" {
				if len(resumed.Execution.WaitingInputs) != 1 || resumed.Execution.WaitingInputs[0].ToolCallID != "auto-root-write" {
					t.Fatalf("new approval lost: %+v", resumed.Execution)
				}
				if _, err = os.Stat(filepath.Join(f.sess.CWD, "auto-root.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("effect without approval: %v", err)
				}
				resumed, err = f.c.ResumeExecution(f.ctx, f.sess.ID, harness.ResumeExecutionRequest{RunID: resumed.Execution.RunID, ExpectedVersion: resumed.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
					return harness.AllowOnce, nil
				})
				if err != nil || resumed.Execution.Status != harness.ExecutionCompleted {
					t.Fatalf("manual approval after wake: %+v %v", resumed, err)
				}
			} else if livePermissions.Load() != 1 {
				t.Fatal("fresh approval did not use live callback once")
			}
			if mode != "cancel" {
				data, err := os.ReadFile(filepath.Join(f.sess.CWD, "auto-root.txt"))
				if err != nil || string(data) != "root once" {
					t.Fatalf("root effect: %q %v", data, err)
				}
			} else if _, err = os.Stat(filepath.Join(f.sess.CWD, "auto-root.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancel allowed effect: %v", err)
			}
			if err = f.c.CloseSession(f.ctx, f.sess.ID); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestACPAutomaticUpdatesReconnectAndFreshApproval(t *testing.T) {
	f := newAutomaticUpdatesFixture(t)
	if err := f.c.CloseSession(f.ctx, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan harness.RunResult, 4)
	type lifecycle struct {
		SessionID, Phase, OperationID, RunID string
		Execution                            *harness.ExecutionState
		Result                               harness.RunResult
		Error                                json.RawMessage
	}
	starts, finishes := make(chan lifecycle, 4), make(chan lifecycle, 4)
	var approvals, automaticNotifications atomic.Int32
	handler := func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "session/request_permission":
			approvals.Add(1)
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
		case "_deerflow/executions/updated":
			var msg lifecycle
			if err := json.Unmarshal(raw, &msg); err != nil {
				return nil, err
			}
			if len(msg.Error) != 0 {
				return nil, fmt.Errorf("automatic ACP error: %s", msg.Error)
			}
			if msg.Phase == "started" {
				starts <- msg
				return nil, nil
			}
			finishes <- msg
			automaticNotifications.Add(1)
			results <- msg.Result
		}
		return nil, nil
	}
	first := newContinuationACPConnection(t, f, handler)
	var initial continuationACPResponse
	if err := first.peer.Call(f.ctx, "session/prompt", map[string]any{"sessionId": f.sess.ID, "prompt": []harness.Content{{Type: "text", Text: "delegate then write"}}}, &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Meta.Deerflow.Execution == nil || len(initial.Meta.Deerflow.Execution.WaitingTasks) != 1 {
		t.Fatalf("ACP did not wait: %+v", initial)
	}
	first.close(t)
	next := newContinuationACPConnection(t, f, handler)
	// Repeated attachment must retain a single pump.
	if err := next.peer.Call(f.ctx, "session/resume", map[string]any{"sessionId": f.sess.ID, "cwd": f.sess.CWD, "mcpServers": []any{}}, nil); err != nil {
		t.Fatal(err)
	}
	var child harness.BackgroundTask
	for {
		var listed struct{ Tasks []harness.BackgroundTask }
		if err := next.peer.Call(f.ctx, "_deerflow/tasks/list", map[string]any{"sessionId": f.sess.ID}, &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Tasks) == 1 && listed.Tasks[0].Interaction != nil && listed.Tasks[0].Interaction.Resumable {
			child = listed.Tasks[0]
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("ACP child wait stalled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := next.peer.Call(f.ctx, "_deerflow/tasks/approve", map[string]any{"sessionId": f.sess.ID, "taskId": child.ID, "approval": harness.TaskApproval{ID: child.Interaction.ID, TaskVersion: child.Version, Decision: harness.AllowOnce}}, nil); err != nil {
		t.Fatal(err)
	}
	var result harness.RunResult
	select {
	case result = <-results:
	case <-f.ctx.Done():
		t.Fatal("ACP automatic notification missing")
	}
	if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.RunID != initial.Meta.Deerflow.Execution.RunID || result.Execution.InputID != initial.Meta.Deerflow.Execution.InputID {
		t.Fatalf("ACP automatic result: %+v", result)
	}
	var start, finish lifecycle
	select {
	case start = <-starts:
	case <-f.ctx.Done():
		t.Fatal("missing automatic start before output")
	}
	select {
	case finish = <-finishes:
	case <-f.ctx.Done():
		t.Fatal("missing automatic completion identity")
	}
	if start.SessionID != f.sess.ID || start.OperationID == "" || start.Execution == nil || start.RunID != result.Execution.RunID || start.Execution.RunID != start.RunID || start.Execution.Version < 1 || start.Execution.AttemptID == "" || (start.Execution.Status != harness.ExecutionRunning && start.Execution.Status != harness.ExecutionResuming) {
		t.Fatalf("invalid committed automatic start: %+v", start)
	}
	if finish.Phase != "finished" || finish.SessionID != start.SessionID || finish.OperationID != start.OperationID || finish.RunID != start.RunID || result.Execution.Version < start.Execution.Version || result.Execution.Attempt <= start.Execution.Attempt {
		t.Fatalf("operation identity did not survive multiple permission attempts: start=%+v finish=%+v", start, finish)
	}
	if err := next.peer.Call(f.ctx, "session/close", map[string]any{"sessionId": f.sess.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if automaticNotifications.Load() != 1 || approvals.Load() != 2 {
		t.Fatalf("duplicate automatic output/approval: %d %d", automaticNotifications.Load(), approvals.Load())
	}
	data, err := os.ReadFile(filepath.Join(f.sess.CWD, "auto-root.txt"))
	if err != nil || string(data) != "root once" {
		t.Fatalf("ACP root effect: %q %v", data, err)
	}
}
