package agent_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/agent"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type engineFunc func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error)

func (f engineFunc) Run(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
	return f(ctx, req, emit, permission)
}

type fixture struct {
	service *hr.Service
	db      *sqlite.Store
	cwd     string
}

func newFixture(t *testing.T, engine harness.Engine) *fixture {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(root, "state", "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	store, err := hr.NewStore(context.Background(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{service: hr.NewService(store, engine, "fake-model"), db: db, cwd: cwd}
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *protocol.Error `json:"error,omitempty"`
}

type client struct {
	agent      *agent.Agent
	in         *os.File
	out        *os.File
	messages   chan wireMessage
	readErrors chan error
	finished   chan struct{}
	serveErr   error
	nextID     int
}

func connect(t *testing.T, service *hr.Service) *client {
	t.Helper()
	agentIn, clientIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	clientOut, agentOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &client{agent: agent.New(service, agentIn, agentOut), in: clientIn, out: clientOut, messages: make(chan wireMessage, 128), readErrors: make(chan error, 1), finished: make(chan struct{})}
	go func() { c.serveErr = c.agent.Serve(context.Background()); close(c.finished) }()
	go func() {
		defer close(c.messages)
		reader := bufio.NewReader(clientOut)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
					c.readErrors <- err
				}
				return
			}
			var msg wireMessage
			if err = json.Unmarshal(line, &msg); err != nil {
				c.readErrors <- fmt.Errorf("stdout is not JSON-RPC: %w", err)
				return
			}
			if msg.JSONRPC != "2.0" {
				c.readErrors <- fmt.Errorf("invalid stdout protocol version %q", msg.JSONRPC)
				return
			}
			select {
			case c.messages <- msg:
			case <-c.finished:
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = c.agent.Close()
		_ = c.in.Close()
		_ = c.out.Close()
		select {
		case <-c.finished:
			if c.serveErr != nil {
				t.Errorf("agent shutdown: %v", c.serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not finish disconnect cleanup")
		}
	})
	return c
}

func (c *client) send(t *testing.T, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.in.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (c *client) request(t *testing.T, method string, params any) int {
	t.Helper()
	c.nextID++
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	return c.nextID
}

func (c *client) notify(t *testing.T, method string, params any) {
	t.Helper()
	c.send(t, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *client) read(t *testing.T) wireMessage {
	t.Helper()
	select {
	case err := <-c.readErrors:
		t.Fatal(err)
	case msg, ok := <-c.messages:
		if !ok {
			t.Fatal("agent stdout closed before expected protocol message")
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ACP message")
	}
	return wireMessage{}
}

func (c *client) response(t *testing.T, id int) wireMessage {
	t.Helper()
	msg := c.read(t)
	if msg.Method != "" || string(msg.ID) != strconv.Itoa(id) {
		t.Fatalf("expected response %d, received %+v", id, msg)
	}
	return msg
}

func (c *client) success(t *testing.T, id int) json.RawMessage {
	t.Helper()
	msg := c.response(t, id)
	if msg.Error != nil {
		t.Fatalf("request %d failed: %v", id, msg.Error)
	}
	return msg.Result
}

func (c *client) initialize(t *testing.T) {
	t.Helper()
	id := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var response struct {
		ProtocolVersion int `json:"protocolVersion"`
		Capabilities    struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.success(t, id), &response); err != nil {
		t.Fatal(err)
	}
	if response.ProtocolVersion != 1 || !response.Capabilities.LoadSession {
		t.Fatalf("initialize response=%+v", response)
	}
}

func (c *client) newSession(t *testing.T, cwd string) string {
	t.Helper()
	id := c.request(t, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	var response struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(c.success(t, id), &response); err != nil {
		t.Fatal(err)
	}
	if response.SessionID == "" {
		t.Fatal("missing session ID")
	}
	return response.SessionID
}

func promptParams(id, text string) map[string]any {
	return map[string]any{"sessionId": id, "prompt": []harness.Content{{Type: "text", Text: text}}}
}

func stopReason(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.StopReason != want {
		t.Fatalf("stop reason=%q want %q", response.StopReason, want)
	}
}

type sessionUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string          `json:"sessionUpdate"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"toolCallId"`
		Status     string          `json:"status"`
	} `json:"update"`
}

func update(t *testing.T, msg wireMessage, sessionID, kind string) sessionUpdate {
	t.Helper()
	if msg.Method != "session/update" || msg.ID != nil {
		t.Fatalf("expected session update: %+v", msg)
	}
	var event sessionUpdate
	if err := json.Unmarshal(msg.Params, &event); err != nil {
		t.Fatal(err)
	}
	if event.SessionID != sessionID || event.Update.Kind != kind {
		t.Fatalf("update=%+v", event)
	}
	return event
}

func chunkText(t *testing.T, event sessionUpdate) string {
	t.Helper()
	var content harness.Content
	if err := json.Unmarshal(event.Update.Content, &content); err != nil {
		t.Fatal(err)
	}
	return content.Text
}

func TestInitializeNewListAndPromptStreaming(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, part := range []string{"Hello", " world"} {
			if err := emit(ctx, harness.RunEvent{Kind: "text_delta", Text: part}); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	before := c.request(t, "session/new", map[string]any{"cwd": f.cwd, "mcpServers": []any{}})
	if msg := c.response(t, before); msg.Error == nil || msg.Error.Code != protocol.InvalidRequest {
		t.Fatalf("pre-init request=%+v", msg)
	}
	c.initialize(t)
	sessionID := c.newSession(t, f.cwd)
	listed := c.request(t, "session/list", map[string]any{})
	var listing struct {
		Sessions []struct {
			ID  string `json:"sessionId"`
			CWD string `json:"cwd"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(c.success(t, listed), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 || listing.Sessions[0].ID != sessionID {
		t.Fatalf("list=%+v", listing)
	}
	prompt := c.request(t, "session/prompt", promptParams(sessionID, "greet"))
	var text string
	for range 2 {
		text += chunkText(t, update(t, c.read(t), sessionID, "agent_message_chunk"))
	}
	stopReason(t, c.success(t, prompt), "end_turn")
	if text != "Hello world" {
		t.Fatalf("stream=%q", text)
	}
	history, err := f.service.Store.History(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Kind != "user_message" || history[0].Content[0].Text != "greet" {
		t.Fatalf("durable history=%+v", history)
	}
}

func TestReversePermissionDuringActivePrompt(t *testing.T) {
	decision := make(chan harness.PermissionDecision, 1)
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		args := json.RawMessage(`{"path":"result.txt","text":"ok"}`)
		if err := emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: "write-1", ToolName: "write_file", Status: "pending", Arguments: args}); err != nil {
			return harness.RunResult{}, err
		}
		approved, err := permission(ctx, harness.PermissionRequest{ToolCallID: "write-1", ToolName: "write_file", Arguments: args})
		decision <- approved
		if err != nil {
			return harness.RunResult{}, err
		}
		if approved != harness.AllowOnce {
			return harness.RunResult{}, harness.ErrPermissionDenied
		}
		if err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: "write-1", Status: "completed", Text: "wrote result"}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sessionID := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sessionID, "write result"))
	started := update(t, c.read(t), sessionID, "tool_call")
	if started.Update.ToolCallID != "write-1" || started.Update.Status != "pending" {
		t.Fatalf("tool start=%+v", started)
	}
	permission := c.read(t)
	if permission.Method != "session/request_permission" || permission.ID == nil {
		t.Fatalf("reverse request=%+v", permission)
	}
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ID string `json:"toolCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(permission.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.SessionID != sessionID || params.ToolCall.ID != "write-1" {
		t.Fatalf("permission=%+v", params)
	}
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": permission.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}})
	completed := update(t, c.read(t), sessionID, "tool_call_update")
	if completed.Update.Status != "completed" {
		t.Fatalf("tool result=%+v", completed)
	}
	stopReason(t, c.success(t, id), "end_turn")
	if got := <-decision; got != harness.AllowOnce {
		t.Fatalf("decision=%s", got)
	}
	var persisted string
	if err := f.db.DB().QueryRow(`SELECT decision FROM harness_approvals WHERE session_id=?`, sessionID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != "allow_once" {
		t.Fatalf("durable decision=%q", persisted)
	}
}

func TestDuplicatePromptPreservesFirstAndCleanupBusyBoundary(t *testing.T) {
	started := make(chan context.Context, 1)
	cleanup := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(cleanup) })
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		started <- ctx
		<-ctx.Done()
		<-cleanup
		return harness.RunResult{}, ctx.Err()
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	first := c.request(t, "session/prompt", promptParams(sid, "first"))
	var runCtx context.Context
	select {
	case runCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not start")
	}
	second := c.request(t, "session/prompt", promptParams(sid, "second"))
	if msg := c.response(t, second); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("duplicate prompt=%+v", msg)
	}
	if runCtx.Err() != nil {
		t.Fatal("duplicate prompt preempted original run")
	}
	c.notify(t, "session/cancel", map[string]any{"sessionId": sid})
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach engine")
	}
	third := c.request(t, "session/prompt", promptParams(sid, "while cleanup runs"))
	if msg := c.response(t, third); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("cleanup admission=%+v", msg)
	}
	release.Do(func() { close(cleanup) })
	stopReason(t, c.success(t, first), "cancelled")
	if calls.Load() != 1 {
		t.Fatalf("engine ran %d times", calls.Load())
	}
}

func TestPromptFollowedImmediatelyByCancelReturnsCancelled(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		<-ctx.Done()
		return harness.RunResult{}, ctx.Err()
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	c.nextID++
	id := c.nextID
	prompt, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": promptParams(sid, "cancel immediately")})
	cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sid}})
	frame := append(append(append(prompt, '\n'), cancel...), '\n')
	if _, err := c.in.Write(frame); err != nil {
		t.Fatal(err)
	}
	stopReason(t, c.success(t, id), "cancelled")
}

func TestLoadReplaysHistoryBeforeResponseAndResumeDoesNot(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "durable answer"}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	first := connect(t, f.service)
	first.initialize(t)
	sid := first.newSession(t, f.cwd)
	prompt := first.request(t, "session/prompt", promptParams(sid, "durable question"))
	update(t, first.read(t), sid, "agent_message_chunk")
	stopReason(t, first.success(t, prompt), "end_turn")
	closed := first.request(t, "session/close", map[string]any{"sessionId": sid})
	first.success(t, closed)
	second := connect(t, f.service)
	second.initialize(t)
	load := second.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	question := update(t, second.read(t), sid, "user_message_chunk")
	answer := update(t, second.read(t), sid, "agent_message_chunk")
	if chunkText(t, question) != "durable question" || chunkText(t, answer) != "durable answer" {
		t.Fatalf("history=%+v %+v", question, answer)
	}
	second.success(t, load)
	closed = second.request(t, "session/close", map[string]any{"sessionId": sid})
	second.success(t, closed)
	resume := second.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	// The very next frame must be the result, with no historical notifications.
	second.success(t, resume)
	listed := second.request(t, "session/list", map[string]any{})
	second.success(t, listed)
}

func TestTwoConnectionsAndEOFKeepOtherSessionRunning(t *testing.T) {
	type activeRun struct {
		sessionID string
		ctx       context.Context
	}
	started := make(chan activeRun, 2)
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		started <- activeRun{req.Session.ID, ctx}
		<-ctx.Done()
		return harness.RunResult{}, ctx.Err()
	}))
	a := connect(t, f.service)
	a.initialize(t)
	aID := a.newSession(t, f.cwd)
	b := connect(t, f.service)
	b.initialize(t)
	bID := b.newSession(t, f.cwd)
	foreignLoad := b.request(t, "session/load", map[string]any{"sessionId": aID, "cwd": f.cwd, "mcpServers": []any{}})
	if msg := b.response(t, foreignLoad); msg.Error == nil || msg.Error.Code != protocol.ServerBusy {
		t.Fatalf("double attachment=%+v", msg)
	}
	a.request(t, "session/prompt", promptParams(aID, "run A"))
	bPrompt := b.request(t, "session/prompt", promptParams(bID, "run B"))
	active := make(map[string]context.Context)
	for range 2 {
		select {
		case run := <-started:
			active[run.sessionID] = run.ctx
		case <-time.After(5 * time.Second):
			t.Fatal("parallel engines did not start")
		}
	}
	b.notify(t, "session/cancel", map[string]any{"sessionId": aID})
	barrier := b.request(t, "session/list", map[string]any{})
	b.success(t, barrier)
	if active[aID].Err() != nil {
		t.Fatal("foreign connection canceled A")
	}
	_ = a.in.Close()
	select {
	case <-a.finished:
		if a.serveErr != nil {
			t.Fatal(a.serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EOF cleanup did not complete")
	}
	if active[aID].Err() == nil {
		t.Fatal("EOF failed to cancel A")
	}
	if active[bID].Err() != nil {
		t.Fatal("A disconnect canceled B")
	}
	b.notify(t, "session/cancel", map[string]any{"sessionId": bID})
	stopReason(t, b.success(t, bPrompt), "cancelled")
	// Only after EOF cleanup may another connection attach A's durable session.
	resume := b.request(t, "session/resume", map[string]any{"sessionId": aID, "cwd": f.cwd, "mcpServers": []any{}})
	b.success(t, resume)
}

func TestEOFWhileAwaitingPermissionRecordsCancellation(t *testing.T) {
	observed := make(chan harness.PermissionDecision, 1)
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		decision, err := permission(ctx, harness.PermissionRequest{ToolCallID: "call", ToolName: "write_file", Arguments: json.RawMessage(`{"path":"x"}`)})
		observed <- decision
		return harness.RunResult{}, err
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	c.request(t, "session/prompt", promptParams(sid, "needs permission"))
	request := c.read(t)
	if request.Method != "session/request_permission" {
		t.Fatalf("request=%+v", request)
	}
	_ = c.in.Close()
	select {
	case decision := <-observed:
		if decision != harness.PermissionCancelled {
			t.Fatalf("decision=%s", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("permission waiter leaked")
	}
	select {
	case <-c.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("agent cleanup leaked")
	}
	var decision, status string
	if err := f.db.DB().QueryRow(`SELECT decision FROM harness_approvals WHERE session_id=?`, sid).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if err := f.db.DB().QueryRow(`SELECT status FROM harness_runs WHERE session_id=?`, sid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if decision != "cancelled" || status != "cancelled" {
		t.Fatalf("persisted approval=%q run=%q", decision, status)
	}
}

func TestSequentialPromptResultsPermitNextTurn(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for i := 0; i < 30; i++ {
		id := c.request(t, "session/prompt", promptParams(sid, fmt.Sprintf("turn %d", i)))
		stopReason(t, c.success(t, id), "end_turn")
	}
	// Every accepted prompt persists its own user input exactly once.
	history, err := f.service.Store.History(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 30 {
		t.Fatalf("history events=%d", len(history))
	}
	encoded, _ := json.Marshal(history)
	if !bytes.Contains(encoded, []byte("turn 29")) {
		t.Fatal("last turn missing from durable history")
	}
}
