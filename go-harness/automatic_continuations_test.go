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
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func automaticRoles(enabled bool, limit int) *harness.SubagentsConfig {
	return &harness.SubagentsConfig{AutoContinue: &enabled, MaxAutoContinuations: &limit, CustomAgents: map[string]harness.AgentConfig{"reviewer": {Description: "review", SystemPrompt: "ROLE_CATALOG_V1", Tools: []string{}}}}
}

func TestSDKAutomaticNotificationSiblingsShareRootQuota(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "fixture", 400)
			return
		}
		lastUser := ""
		tools := 0
		for _, m := range req.Messages {
			if m.Role == "user" {
				lastUser = string(m.Content)
			}
			if m.Role == "tool" {
				tools++
			}
		}
		message := map[string]any{"role": "assistant", "content": "finished"}
		finish := "stop"
		if !strings.Contains(lastUser, "Background task notification.") && !strings.Contains(lastUser, "SIBLING_CHILD") && tools == 0 {
			calls := []any{}
			for i := 0; i < 2; i++ {
				calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("sibling-%d", i), "type": "function", "function": map[string]string{"name": "background_agent", "arguments": fmt.Sprintf(`{"instruction":"SIBLING_CHILD_%d"}`, i)}})
			}
			message["content"], message["tool_calls"] = nil, calls
			finish = "tool_calls"
		}
		writeTreeModelResponse(w, req.Stream, message, finish)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	enabled, limit := true, 1
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 2, PermissionMode: harness.PermissionModeOff, Subagents: &harness.SubagentsConfig{AutoContinue: &enabled, MaxAutoContinuations: &limit}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "two tasks"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &continuationFixture{c: c, ctx: ctx, sess: sess, parent: parent}
	results, done, stop := startContinuationUpdates(t, f, nil)
	select {
	case r := <-results:
		if r.Execution == nil || r.Execution.Status != harness.ExecutionCompleted {
			t.Fatalf("first sibling: %+v", r)
		}
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("sibling continuation stalled")
	}
	second := ""
	for second == "" {
		notes, err := c.BackgroundNotifications(ctx, sess.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range notes {
			if n.Kind == "completed" && n.Delivery != nil && n.Delivery.Status == "limit_reached" {
				second = n.ID
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("shared quota never blocked second sibling")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var used, sources int
	if err = c.store.DB().QueryRow(`SELECT used,(SELECT count(*) FROM harness_continuation_sources) FROM harness_automatic_continuations WHERE root_id=?`, parent.Execution.RunID).Scan(&used, &sources); err != nil || used != 1 || sources != 1 {
		t.Fatalf("siblings bypassed quota: %d %d %v", used, sources, err)
	}
	manual, err := c.ProcessBackgroundNotification(ctx, sess.ID, second, nil, nil)
	if err != nil || manual.Execution == nil || manual.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("explicit manual recovery: %+v %v", manual, err)
	}
}

func TestSDKAutomaticNotificationAdmissionFailureIsAtomicAndRetryable(t *testing.T) {
	f := newContinuationRoleFixture(t, false, automaticRoles(true, 2))
	if _, err := f.c.store.DB().Exec(`CREATE TRIGGER auto_admit_fault BEFORE INSERT ON harness_events WHEN json_extract(CAST(NEW.event AS TEXT),'$.kind')='continuation_started' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	errorsSeen := make(chan error, 4)
	results := make(chan harness.RunResult, 2)
	done := make(chan error, 1)
	go func() {
		done <- f.c.ServeSessionUpdates(ctx, f.sess.ID, harness.SessionUpdates{Result: func(_ context.Context, result harness.RunResult, err error) error {
			if err != nil {
				errorsSeen <- err
			} else {
				results <- result
			}
			return nil
		}})
	}()
	select {
	case <-errorsSeen:
	case err := <-done:
		t.Fatal(err)
	case <-f.ctx.Done():
		t.Fatal("injected failure not reported")
	}
	var sources, roots, attempts int
	if err := f.c.store.DB().QueryRow(`SELECT (SELECT count(*) FROM harness_continuation_sources),(SELECT count(*) FROM harness_automatic_continuations),(SELECT attempts FROM harness_notification_work WHERE notification_id=?)`, f.note.ID).Scan(&sources, &roots, &attempts); err != nil || sources != 0 || roots != 0 || attempts != 1 {
		t.Fatalf("partial admission: sources=%d roots=%d attempts=%d err=%v", sources, roots, attempts, err)
	}
	if _, err := f.c.store.DB().Exec(`DROP TRIGGER auto_admit_fault`); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
			t.Fatalf("retry result: %+v", result)
		}
	case err := <-done:
		t.Fatal(err)
	case <-f.ctx.Done():
		t.Fatal("admission retry stalled")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var used int
	if err := f.c.store.DB().QueryRow(`SELECT used FROM harness_automatic_continuations WHERE root_id=?`, f.parent.Execution.RunID).Scan(&used); err != nil || used != 1 {
		t.Fatalf("retry quota %d %v", used, err)
	}
}

func TestACPAutomaticNotificationAdmissionAndProjection(t *testing.T) {
	f := newContinuationRoleFixture(t, true, automaticRoles(true, 2))
	if err := f.c.CloseSession(f.ctx, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan harness.RunResult, 2)
	conn := newContinuationACPConnection(t, f, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "session/request_permission" {
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
		}
		if method == "_deerflow/executions/updated" {
			var msg struct {
				Phase  string
				Result harness.RunResult
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				return nil, err
			}
			if msg.Phase == "finished" {
				results <- msg.Result
			}
		}
		return nil, nil
	})
	var result harness.RunResult
	select {
	case result = <-results:
	case <-f.ctx.Done():
		t.Fatal("ACP notification admission stalled")
	}
	if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.RunID == f.parent.Execution.RunID {
		t.Fatalf("ACP continuation: %+v", result)
	}
	var list struct {
		Notifications []harness.BackgroundNotification
	}
	if err := conn.peer.Call(f.ctx, "_deerflow/notifications/list", map[string]any{"sessionId": f.sess.ID}, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range list.Notifications {
		if n.ID == f.note.ID {
			found = n.Delivery != nil && n.Delivery.Status == "admitted" && n.Delivery.RunID == result.Execution.RunID
		}
	}
	if !found {
		t.Fatalf("missing ACP admission state: %+v", list)
	}
	conn.close(t)
}

func startContinuationUpdates(t *testing.T, f *continuationFixture, permission harness.PermissionHandler) (<-chan harness.RunResult, <-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	results := make(chan harness.RunResult, 8)
	done := make(chan error, 1)
	go func() {
		done <- f.c.ServeSessionUpdates(ctx, f.sess.ID, harness.SessionUpdates{Permission: permission, Result: func(_ context.Context, result harness.RunResult, err error) error {
			if err == nil {
				results <- result
			}
			return err
		}})
	}()
	return results, done, cancel
}

func TestSDKAutomaticNotificationRestartPolicyAndIndependentAck(t *testing.T) {
	f := newContinuationRoleFixture(t, true, automaticRoles(true, 2))
	if err := f.c.AcknowledgeBackgroundNotification(f.ctx, f.sess.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	// Current settings affect future roots. The accepted source retains its
	// original catalog, continuation policy, input and shared budget.
	f.cfg.Subagents = automaticRoles(false, 0)
	f.reopen(t)
	results, done, cancel := startContinuationUpdates(t, f, nil)
	var result harness.RunResult
	select {
	case result = <-results:
	case err := <-done:
		t.Fatalf("updates: %v", err)
	case <-f.ctx.Done():
		t.Fatal("automatic notification stalled")
	}
	if result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || result.Execution.RunID == f.parent.Execution.RunID || result.Execution.InputID == f.parent.Execution.InputID || len(result.Execution.WaitingInputs) != 1 {
		t.Fatalf("automatic acceptance/approval: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(f.sess.CWD, "continued.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic write without approval: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	resumed, err := f.c.ResumeExecution(f.ctx, f.sess.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || resumed.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("continuation approval after restart: %+v %v", resumed, err)
	}
	var used, inputs int
	if err = f.c.store.DB().QueryRow(`SELECT used FROM harness_automatic_continuations WHERE root_id=?`, f.parent.Execution.RunID).Scan(&used); err != nil || used != 1 {
		t.Fatalf("quota reset/double-spent: %d %v", used, err)
	}
	if err = f.c.store.DB().QueryRow(`SELECT count(*) FROM harness_continuation_sources`).Scan(&inputs); err != nil || inputs != 1 {
		t.Fatalf("duplicate accepted input: %d %v", inputs, err)
	}
	calls := f.calls.Load()
	_, err = f.c.ProcessBackgroundNotification(f.ctx, f.sess.ID, f.note.ID, nil, nil)
	if err != nil || f.calls.Load() != calls {
		t.Fatalf("automatic source was not idempotent: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(f.sess.CWD, "continued.txt"))
	if err != nil || string(data) != "one continuation effect" {
		t.Fatalf("effect: %q %v", data, err)
	}
}

func waitNotificationState(t *testing.T, f *continuationFixture, want string) {
	t.Helper()
	for {
		notes, err := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, note := range notes {
			if note.ID == f.note.ID && note.Delivery != nil {
				if note.Delivery.Status == want {
					return
				}
				if note.Delivery.Status != "queued" {
					t.Fatalf("delivery: %+v", note.Delivery)
				}
			}
		}
		select {
		case <-f.ctx.Done():
			t.Fatalf("missing delivery state %s", want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSDKAutomaticNotificationDisabledStoppedAndQuotaZero(t *testing.T) {
	for _, mode := range []string{"disabled", "stopped", "limit_reached", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			limit := 2
			if mode == "limit_reached" {
				limit = 0
			}
			var f *continuationFixture
			if mode == "legacy" {
				f = newContinuationScenarioFixture(t, false, nil, true)
				f.cfg.Subagents = automaticRoles(true, 2)
				f.reopen(t)
			} else {
				f = newContinuationRoleFixture(t, false, automaticRoles(mode != "disabled", limit))
			}
			if mode == "stopped" {
				if err := f.c.Cancel(f.sess.ID); err != nil {
					t.Fatal(err)
				}
				f.reopen(t)
			}
			before := f.calls.Load()
			_, done, cancel := startContinuationUpdates(t, f, nil)
			want := mode
			if mode == "legacy" {
				want = "disabled"
			}
			waitNotificationState(t, f, want)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if f.calls.Load() != before {
				t.Fatal("disallowed notification invoked a model")
			}
		})
	}
}

func TestSDKAutomaticWaitDoesNotRedeliverNotification(t *testing.T) {
	f := newAutomaticUpdatesFixtureWithPolicy(t, true)
	parent, err := f.c.Run(f.ctx, f.sess.ID, []harness.Content{{Type: "text", Text: "delegate then wait"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || parent.Execution == nil || len(parent.Execution.WaitingTasks) != 1 {
		t.Fatalf("parent: %+v %v", parent, err)
	}
	child := waitingAutomaticChild(t, f)
	results, done, cancel := startContinuationUpdates(t, f, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if _, err = f.c.ApproveBackgroundTask(f.ctx, f.sess.ID, child.ID, harness.TaskApproval{ID: child.Interaction.ID, TaskVersion: child.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.RunID != parent.Execution.RunID {
			t.Fatalf("wait: %+v", result)
		}
	case err := <-done:
		t.Fatal(err)
	case <-f.ctx.Done():
		t.Fatal("wait stalled")
	}
	for f.note.ID == "" {
		notes, e := f.c.BackgroundNotifications(f.ctx, f.sess.ID, 0, 100)
		if e != nil {
			t.Fatal(e)
		}
		for _, note := range notes {
			if note.TaskID == child.ID && note.Kind == "completed" {
				f.note = note
			}
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("no terminal note")
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitNotificationState(t, f, "consumed")
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.c.store.DB().QueryRow(`SELECT count(*) FROM harness_continuation_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("wait created duplicate continuation %d %v", count, err)
	}
}
