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
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// This fixture uses the public SDK and real Eino TurnLoop, with a local model
// endpoint. Continuation requests are distinguished by their accepted source,
// never by a caller-provided prompt or by a second delegation result.
type continuationFixture struct {
	c      *Client
	cfg    Config
	ctx    context.Context
	sess   harness.Session
	parent harness.RunResult
	task   harness.BackgroundTask
	note   harness.BackgroundNotification
	calls  atomic.Int32
	models atomic.Int32
}

func newContinuationFixture(t *testing.T, write bool, modelLimit ...int) *continuationFixture {
	t.Helper()
	f := &continuationFixture{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				ToolCallID string          `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture input", http.StatusBadRequest)
			return
		}
		f.calls.Add(1)
		lastUser, originalCount, delegateResults, hasWrite := "", 0, 0, false
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				lastUser = string(msg.Content)
				if strings.Contains(lastUser, "ORIGINAL_PARENT_PROMPT") {
					originalCount++
				}
			}
			if msg.Role == "tool" && msg.ToolCallID == "delegate-once" {
				delegateResults++
			}
			if msg.Role == "tool" && msg.ToolCallID == "continuation-write" {
				hasWrite = true
			}
		}
		name, id, args, content := "", "", "", ""
		switch {
		case strings.Contains(lastUser, "Background task notification."):
			f.models.Add(1)
			if strings.Contains(lastUser, "ORIGINAL_PARENT_PROMPT") || originalCount > 1 || delegateResults > 1 {
				t.Errorf("continuation replayed origin input/result: original=%d delegation=%d last=%s", originalCount, delegateResults, lastUser)
			}
			if !strings.Contains(lastUser, "CHILD_RESULT") {
				t.Errorf("model did not receive readable child result: %s", lastUser)
			}
			if write && !hasWrite {
				name, id, args = "write_file", "continuation-write", `{ "path": "continued.txt", "content": "one continuation effect" }`
			} else {
				content = "CONTINUATION_COMPLETE"
			}
		case strings.Contains(lastUser, "CHILD_INSTRUCTION"):
			content = "CHILD_RESULT: observed durable child completion"
		case strings.Contains(lastUser, "UNRELATED_NEW_PROMPT"):
			content = "UNRELATED_COMPLETE"
		case delegateResults == 0:
			name, id, args = "background_agent", "delegate-once", `{"instruction":"CHILD_INSTRUCTION: return the observation","description":"child observation"}`
		default:
			content = "PARENT_SUBMITTED"
		}
		finish := "stop"
		message := map[string]any{"role": "assistant", "content": content}
		if name != "" {
			finish = "tool_calls"
			call := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
			if req.Stream {
				call["index"] = 0
			}
			message["content"] = nil
			message["tool_calls"] = []any{call}
		}
		usage := map[string]int{"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30,\"total_tokens\":80}}\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(provider.Close)
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	limits := harness.BudgetLimits{MaxModelCalls: 12, MaxToolCalls: 4, MaxTokens: 100000, MaxOutputTokens: 256}
	if len(modelLimit) > 0 {
		limits.MaxModelCalls = modelLimit[0]
	}
	f.cfg = Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, Budget: &limits}
	var err error
	f.c, err = Open(f.ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.c != nil {
			if err := f.c.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	f.sess, err = f.c.NewSession(f.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.parent, err = f.c.Run(f.ctx, f.sess.ID, []harness.Content{{Type: "text", Text: "ORIGINAL_PARENT_PROMPT: delegate an observation"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if p.ToolName != "background_agent" {
			t.Errorf("unexpected original approval: %s", p.ToolName)
		}
		return harness.AllowOnce, nil
	})
	if err != nil || f.parent.Execution == nil || f.parent.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("parent=%+v err=%v", f.parent, err)
	}
	tasks, err := f.c.BackgroundTasks(f.ctx, f.sess.ID, "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	f.task = tasks[0]
	for f.task.Status == "pending" || f.task.Status == "running" {
		f.task, err = f.c.WaitBackgroundTask(f.ctx, f.sess.ID, f.task.ID, f.task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.task.Status != "completed" {
		t.Fatalf("child=%+v", f.task)
	}
	for f.note.ID == "" {
		notes, listErr := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, note := range notes {
			if note.TaskID == f.task.ID && note.TaskVersion == f.task.Version {
				f.note = note
				break
			}
		}
		if f.note.ID == "" {
			select {
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return f
}

func (f *continuationFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	var err error
	f.c, err = Open(f.ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.LoadSession(f.ctx, f.sess.ID, f.sess.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSDKNotificationContinuationUsesOriginalBudgetAndIndependentAck(t *testing.T) {
	f := newContinuationFixture(t, false)
	originalRoot := f.parent.Execution.RunID
	before, err := f.c.budgets.Snapshot(f.ctx, originalRoot)
	if err != nil || before.ModelCalls != 3 || before.ToolCalls != 1 {
		t.Fatalf("original budget=%+v err=%v", before, err)
	}
	// A later user prompt must not donate its new root to this notification.
	unrelated, err := f.c.Run(f.ctx, f.sess.ID, []harness.Content{{Type: "text", Text: "UNRELATED_NEW_PROMPT"}}, nil, nil)
	if err != nil || unrelated.Execution == nil || unrelated.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("unrelated=%+v err=%v", unrelated, err)
	}
	if err := f.c.AcknowledgeBackgroundNotification(f.ctx, f.sess.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	result, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("continuation=%+v err=%v", result, err)
	}
	if result.Execution.RunID == originalRoot || result.Execution.InputID == f.parent.Execution.InputID || result.Execution.RunID == unrelated.Execution.RunID {
		t.Fatalf("continuation reused accepted run/input identity: %+v", result.Execution)
	}
	count := f.calls.Load()
	f.reopen(t)
	repeat, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || repeat.Execution == nil || repeat.Execution.RunID != result.Execution.RunID || repeat.Execution.Status != harness.ExecutionCompleted || f.calls.Load() != count || f.models.Load() != 1 {
		t.Fatalf("duplicate=%+v err=%v calls=%d/%d", repeat, err, f.calls.Load(), count)
	}
	budget, err := f.c.budgets.Snapshot(f.ctx, originalRoot)
	if err != nil || budget.ModelCalls != 4 || budget.ToolCalls != 1 || budget.HeldTokens != 0 {
		t.Fatalf("continuation original budget=%+v err=%v", budget, err)
	}
	other, err := f.c.budgets.Snapshot(f.ctx, unrelated.Execution.RunID)
	if err != nil || other.ModelCalls != 1 || other.ToolCalls != 0 {
		t.Fatalf("unrelated root changed=%+v err=%v", other, err)
	}
	var roots int
	if err := f.c.store.DB().QueryRowContext(f.ctx, "SELECT COUNT(*) FROM budget_roots").Scan(&roots); err != nil || roots != 2 {
		t.Fatalf("notification created new budget root: count=%d err=%v", roots, err)
	}
	page, err := f.c.HistoryPage(f.ctx, f.sess.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	users, starts := 0, 0
	for _, event := range page.Events {
		if event.Kind == "user_message" {
			users++
		}
		if event.Kind == "continuation_started" {
			starts++
		}
	}
	if users != 2 || starts != 1 {
		t.Fatalf("domain history users=%d continuations=%d", users, starts)
	}
}

func TestSDKNotificationContinuationNilApprovalWaitsAndResumesAfterRestart(t *testing.T) {
	f := newContinuationFixture(t, true)
	result, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || !result.Execution.Resumable || len(result.Execution.WaitingInputs) != 1 {
		t.Fatalf("continuation did not durably wait=%+v err=%v", result, err)
	}
	before := *result.Execution
	if _, err := os.Stat(filepath.Join(f.sess.CWD, "continued.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original tool approval leaked to continuation: %v", err)
	}
	// Model delivery must not acknowledge the UI inbox.
	notes, err := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
	found := false
	for _, note := range notes {
		found = found || note.ID == f.note.ID
	}
	if err != nil || !found {
		t.Fatalf("process acknowledged inbox: notes=%+v err=%v", notes, err)
	}
	f.reopen(t)
	count := f.calls.Load()
	repeat, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || repeat.Execution == nil || repeat.Execution.RunID != before.RunID || repeat.Execution.Status != harness.ExecutionWaitingInput || f.calls.Load() != count {
		t.Fatalf("waiting repeat=%+v err=%v", repeat, err)
	}
	approved := 0
	result, err = f.c.ResumeExecution(f.ctx, f.sess.ID, harness.ResumeExecutionRequest{RunID: before.RunID, ExpectedVersion: before.Version}, nil, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
		approved++
		if req.ToolName != "write_file" || req.ToolCallID != "continuation-write" || req.RunID != before.RunID || string(req.Arguments) != `{ "path": "continued.txt", "content": "one continuation effect" }` {
			t.Errorf("changed continuation permission=%+v", req)
		}
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.RunID != before.RunID || result.Execution.InputID != before.InputID || approved != 1 {
		t.Fatalf("resume=%+v approvals=%d err=%v", result, approved, err)
	}
	data, err := os.ReadFile(filepath.Join(f.sess.CWD, "continued.txt"))
	if err != nil || string(data) != "one continuation effect" {
		t.Fatalf("effect=%q err=%v", data, err)
	}
	receipts, err := f.c.ListToolReceipts(f.ctx, f.sess.ID)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.State != harness.ReceiptCompleted {
			t.Fatalf("unfinished receipt=%+v", receipt)
		}
	}
	budget, err := f.c.budgets.Snapshot(f.ctx, f.parent.Execution.RunID)
	if err != nil || budget.ToolCalls != 2 || budget.ModelCalls != 5 || budget.HeldTokens != 0 || f.models.Load() != 2 {
		t.Fatalf("shared continuation budget=%+v models=%d err=%v", budget, f.models.Load(), err)
	}
}

func TestSDKNotificationContinuationRejectsChangedHostPolicy(t *testing.T) {
	f := newContinuationFixture(t, false)
	f.cfg.Instruction = "changed host policy"
	f.reopen(t)
	count := f.calls.Load()
	result, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err == nil || result.Execution != nil || f.calls.Load() != count {
		t.Fatalf("accepted changed policy=%+v err=%v", result, err)
	}
	notes, err := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
	if err != nil || len(notes) == 0 {
		t.Fatalf("policy conflict consumed notification: %+v %v", notes, err)
	}
}

func TestSDKNotificationContinuationExhaustedOriginalBudgetRollsBackAdmission(t *testing.T) {
	f := newContinuationFixture(t, false, 3)
	count := f.calls.Load()
	_, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	var limit *budget.LimitError
	if !errors.As(err, &limit) || limit.Resource != "model_calls" || f.calls.Load() != count {
		t.Fatalf("admitted exhausted budget: err=%v modelCalls=%d/%d", err, f.calls.Load(), count)
	}
	var sources, roots int
	if err := f.c.store.DB().QueryRowContext(f.ctx, "SELECT COUNT(*) FROM harness_continuation_sources").Scan(&sources); err != nil || sources != 0 {
		t.Fatalf("exhausted admission saved source: count=%d err=%v", sources, err)
	}
	if err := f.c.store.DB().QueryRowContext(f.ctx, "SELECT COUNT(*) FROM budget_roots").Scan(&roots); err != nil || roots != 1 {
		t.Fatalf("exhausted admission created root: count=%d err=%v", roots, err)
	}
	notes, err := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
	if err != nil || len(notes) == 0 {
		t.Fatalf("exhausted admission consumed inbox: notes=%+v err=%v", notes, err)
	}
}

func TestSDKNotificationContinuationSourceTamperBlocksResume(t *testing.T) {
	f := newContinuationFixture(t, true)
	result, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatalf("initial continuation=%+v err=%v", result, err)
	}
	before := *result.Execution
	providerCalls := f.calls.Load()
	if _, err := f.c.store.DB().ExecContext(f.ctx, "UPDATE harness_continuation_sources SET payload=? WHERE run_id=?", []byte(`{}`), before.RunID); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	state, err := f.c.Execution(f.ctx, f.sess.ID, before.RunID)
	if err != nil || state.Status != harness.ExecutionNeedsReconciliation || state.Resumable || state.BlockedReason == "" {
		t.Fatalf("tampered source presented as resumable=%+v err=%v", state, err)
	}
	_, err = f.c.ResumeExecution(f.ctx, f.sess.ID, harness.ResumeExecutionRequest{RunID: before.RunID, ExpectedVersion: before.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("tampered source reached approval")
		return harness.AllowOnce, nil
	})
	if !errors.Is(err, harness.ErrExecutionConflict) || f.calls.Load() != providerCalls {
		t.Fatalf("tampered source reached provider: err=%v calls=%d/%d", err, f.calls.Load(), providerCalls)
	}
	if _, err := os.Stat(filepath.Join(f.sess.CWD, "continued.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered source reached tool: %v", err)
	}
}

func TestSDKNotificationContinuationWaitingHostDriftIsNotResumable(t *testing.T) {
	f := newContinuationFixture(t, true)
	result, err := f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatalf("initial continuation=%+v err=%v", result, err)
	}
	f.cfg.Instruction = "different process policy"
	f.reopen(t)
	state, err := f.c.Execution(f.ctx, f.sess.ID, result.Execution.RunID)
	if err != nil || state.Status != harness.ExecutionWaitingInput || state.Resumable || !strings.Contains(state.BlockedReason, "host policy changed") {
		t.Fatalf("changed host policy advertised resume=%+v err=%v", state, err)
	}
	calls := f.calls.Load()
	_, err = f.c.ResumeExecution(f.ctx, f.sess.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, nil)
	if !errors.Is(err, harness.ErrExecutionUnresumable) || f.calls.Load() != calls {
		t.Fatalf("changed host policy resumed: err=%v calls=%d/%d", err, f.calls.Load(), calls)
	}
}
