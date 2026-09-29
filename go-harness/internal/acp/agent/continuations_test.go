package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const processNotificationMethod = "_deerflow/notifications/process"

type notificationProcessorStub struct {
	*backgroundControllerStub
	process func(context.Context, harness.TaskActor, string, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error)
}

func (s *notificationProcessorStub) ProcessBackgroundNotification(ctx context.Context, actor harness.TaskActor, id string, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	if err := s.record(ctx, backgroundCall{Method: "process", Actor: actor, ID: id}); err != nil {
		return harness.RunResult{}, err
	}
	if s.process != nil {
		return s.process(ctx, actor, id, emit, approve)
	}
	return harness.RunResult{StopReason: "end_turn"}, nil
}

func notificationFixture(t *testing.T) (*fixture, *notificationProcessorStub, *client, string) {
	t.Helper()
	f := newFixture(t, nil)
	stub := &notificationProcessorStub{backgroundControllerStub: &backgroundControllerStub{service: f.service}}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	return f, stub, c, c.newSession(t, f.cwd)
}

func TestACPNotificationProcessCapabilityAndPreinitialize(t *testing.T) {
	for _, mode := range []string{"no-background", "management-only", "processor"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil)
			base := &backgroundControllerStub{service: f.service}
			switch mode {
			case "management-only":
				f.service.Background = base
			case "processor":
				f.service.Background = &notificationProcessorStub{backgroundControllerStub: base}
			}
			c := connect(t, f.service)
			params := map[string]any{"sessionId": "session", "notificationId": "notification"}
			msg := c.response(t, c.request(t, processNotificationMethod, params))
			if msg.Error == nil || msg.Error.Code != protocol.InvalidRequest {
				t.Fatalf("preinitialize processing=%+v", msg)
			}
			init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
			if bytes.Contains(init, []byte(`"processMethod":"_deerflow/notifications/process"`)) != (mode == "processor") {
				t.Fatalf("optional process capability=%s", init)
			}
			if mode != "processor" {
				msg := c.response(t, c.request(t, processNotificationMethod, params))
				if msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
					t.Fatalf("unsupported processing=%+v", msg)
				}
			}
		})
	}
}

func TestACPNotificationProcessRejectsUntrustedParameters(t *testing.T) {
	_, stub, c, sid := notificationFixture(t)
	for _, body := range []string{
		``, `,"notificationId":null`, `,"notificationId":""`, `,"notificationId":" n"`,
		`,"notificationId":1`, `,"NotificationId":"n"`, `,"notificationId":"n","notificationId":"other"`,
		`,"notificationId":"n","ownerId":"spoof"`, `,"notificationId":"n","prompt":"new prompt"`,
		`,"notificationId":"n","rootBudgetId":"new-root"`, `,"notificationId":"n","originRunId":"other"`,
		`,"notificationId":"n","runId":"other"`, `,"notificationId":"n","inputId":"other"`,
		`,"notificationId":"n","approval":{"decision":"allow_once"}`, `,"notificationId":"n","targets":{}`,
		`,"notificationId":"n","version":1`, `,"notificationId":"n","taskId":"task"`,
	} {
		t.Run(body, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"sessionId":%q%s}`, sid, body))
			msg := c.response(t, c.request(t, processNotificationMethod, raw))
			if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
				t.Fatalf("malformed continuation accepted=%+v", msg)
			}
		})
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{"notificationId":"n"}`), json.RawMessage(`{"sessionId":null,"notificationId":"n"}`), json.RawMessage(`{"sessionId":"s","sessionId":"s","notificationId":"n"}`)} {
		msg := c.response(t, c.request(t, processNotificationMethod, raw))
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("invalid identity=%+v", msg)
		}
	}
	if calls := stub.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid processing reached controller=%+v", calls)
	}
	// Invalid requests must not reserve or poison the current foreground lease.
	c.success(t, c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "valid"}))
}

func TestACPNotificationProcessOwnerAndNoImplicitProcessing(t *testing.T) {
	f, stub, c, sid := notificationFixture(t)
	stranger := connect(t, f.service)
	stranger.initialize(t)
	params := map[string]any{"sessionId": sid, "notificationId": "n"}
	msg := stranger.response(t, stranger.request(t, processNotificationMethod, params))
	if msg.Error == nil {
		t.Fatal("foreign connection processed notification")
	}
	c.notify(t, processNotificationMethod, params)
	c.success(t, c.request(t, "_deerflow/notifications/list", map[string]any{"sessionId": sid}))
	c.success(t, c.request(t, "_deerflow/notifications/ack", params))
	c.success(t, c.request(t, "_deerflow/tasks/wait", map[string]any{"sessionId": sid, "taskId": "task"}))
	for _, call := range stub.snapshot() {
		if call.Method == "process" {
			t.Fatalf("read/ack/notification started model processing: %+v", call)
		}
	}
	c.success(t, c.request(t, processNotificationMethod, params))
	calls := stub.snapshot()
	last := calls[len(calls)-1]
	if last.Method != "process" || last.ID != "n" || last.Actor.SessionID != sid || last.Actor.OwnerID == "" {
		t.Fatalf("processing identity=%+v", last)
	}
}

func TestACPNotificationProcessUsesUpdatesPermissionAndExecutionMetadata(t *testing.T) {
	_, stub, c, sid := notificationFixture(t)
	var effects atomic.Int32
	stub.process = func(ctx context.Context, actor harness.TaskActor, id string, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{SessionID: actor.SessionID, RunID: "continuation", Kind: "text_delta", Text: "Background result received"}); err != nil {
			return harness.RunResult{}, err
		}
		if err := emit(ctx, harness.RunEvent{SessionID: actor.SessionID, RunID: "continuation", Kind: "usage", Usage: &harness.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}); err != nil {
			return harness.RunResult{}, err
		}
		decision, err := approve(ctx, harness.PermissionRequest{ID: "intent", SessionID: actor.SessionID, RunID: "continuation", ToolCallID: "write", ToolName: "write_file", Arguments: json.RawMessage(`{"path":"result.txt","content":"summary"}`)})
		if err != nil {
			return harness.RunResult{}, err
		}
		if decision != harness.AllowOnce {
			return harness.RunResult{}, fmt.Errorf("unexpected decision: %s", decision)
		}
		effects.Add(1)
		return harness.RunResult{StopReason: "end_turn", Execution: &harness.ExecutionState{SessionID: actor.SessionID, RunID: "continuation", InputID: "notification-input", Status: harness.ExecutionCompleted, Version: 3}}, nil
	}
	id := c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "n"})
	chunk := update(t, c.read(t), sid, "agent_message_chunk")
	if chunkText(t, chunk) != "Background result received" {
		t.Fatalf("continuation chunk=%+v", chunk)
	}
	permission := executionPermissionMessage(t, c)
	if !bytes.Contains(permission.Params, []byte(`result.txt`)) || effects.Load() != 0 {
		t.Fatalf("permission/effect=%s effects=%d", permission.Params, effects.Load())
	}
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}})
	result := executionPromptResult(t, c, id)
	if !bytes.Contains(result, []byte(`"runId":"continuation"`)) || !bytes.Contains(result, []byte(`"status":"completed"`)) || !bytes.Contains(result, []byte(`"totalTokens":5`)) || effects.Load() != 1 {
		t.Fatalf("continuation response=%s effects=%d", result, effects.Load())
	}
}

func TestACPNotificationProcessReaderAdmissionAndCancelCleanup(t *testing.T) {
	_, stub, c, sid := notificationFixture(t)
	started := make(chan context.Context, 1)
	cleanup := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(cleanup) })
	stub.process = func(ctx context.Context, _ harness.TaskActor, _ string, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		started <- ctx
		<-ctx.Done()
		<-cleanup
		return harness.RunResult{StopReason: "cancelled"}, nil
	}
	first := c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "n"})
	// Send the competing prompt immediately, before waiting for async dispatch.
	second := c.request(t, "session/prompt", promptParams(sid, "must not preempt"))
	if msg := c.response(t, second); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("reader admission=%+v", msg)
	}
	var runCtx context.Context
	select {
	case runCtx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("continuation did not start")
	}
	if runCtx.Err() != nil {
		t.Fatal("competing prompt preempted continuation")
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	select {
	case <-runCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach continuation")
	}
	if !errors.Is(context.Cause(runCtx), session.ErrExplicitCancel) {
		t.Fatalf("cancel cause=%v", context.Cause(runCtx))
	}
	third := c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "another"})
	if msg := c.response(t, third); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("cleanup admission=%+v", msg)
	}
	release.Do(func() { close(cleanup) })
	stopReason(t, c.success(t, first), "cancelled")
	if calls := stub.snapshot(); len(calls) != 1 {
		t.Fatalf("duplicate processing=%+v", calls)
	}
}

func TestACPNotificationProcessDoesNotPreemptExistingPrompt(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		close(started)
		<-ctx.Done()
		return harness.RunResult{}, ctx.Err()
	}))
	stub := &notificationProcessorStub{backgroundControllerStub: &backgroundControllerStub{service: f.service}}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	prompt := c.request(t, "session/prompt", promptParams(sid, "ongoing"))
	process := c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "n"})
	if msg := c.response(t, process); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("processing preempted prompt=%+v", msg)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("original prompt did not start")
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	stopReason(t, c.success(t, prompt), "cancelled")
	if calls := stub.snapshot(); len(calls) != 0 {
		t.Fatalf("busy processor reached controller=%+v", calls)
	}
}

func TestACPNotificationProcessConflictsUseExecutionRecovery(t *testing.T) {
	_, stub, c, sid := notificationFixture(t)
	for _, test := range []struct {
		err  error
		code int
	}{
		{harness.ErrExecutionConflict, -32013}, {harness.ErrTaskOriginConflict, -32013},
		{harness.ErrExecutionWaitingInput, -32012}, {harness.ErrExecutionUnresumable, -32014}, {harness.ErrBackgroundUncertain, -32014},
	} {
		stub.mu.Lock()
		stub.err = test.err
		stub.mu.Unlock()
		msg := c.response(t, c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "n"}))
		if msg.Error == nil || msg.Error.Code != test.code || !strings.Contains(fmt.Sprint(msg.Error.Data), "_deerflow/executions/get") {
			t.Fatalf("continuation recovery for %v=%+v", test.err, msg)
		}
	}
}

func TestACPNotificationProcessReportsOriginBudgetLimit(t *testing.T) {
	_, stub, c, sid := notificationFixture(t)
	stub.mu.Lock()
	stub.err = &budget.LimitError{Resource: "model_calls"}
	stub.mu.Unlock()
	msg := c.response(t, c.request(t, processNotificationMethod, map[string]any{"sessionId": sid, "notificationId": "n"}))
	if msg.Error == nil || msg.Error.Code != -32015 || !strings.Contains(fmt.Sprint(msg.Error.Data), "model_calls") {
		t.Fatalf("origin budget error=%+v", msg)
	}
}

var _ harness.BackgroundNotificationProcessor = (*notificationProcessorStub)(nil)
