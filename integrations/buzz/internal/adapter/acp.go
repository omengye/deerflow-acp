package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message) }

type rpcResult struct {
	Result json.RawMessage
	Err    error
}
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type rpcPeer struct {
	reader     io.ReadCloser
	writer     io.WriteCloser
	mu         sync.Mutex
	writeMu    sync.Mutex
	next       uint64
	pending    map[string]chan rpcResult
	done       chan struct{}
	err        error
	closed     sync.Once
	onUpdate   func(json.RawMessage)
	permission string
	schedules  bool
}

func newPeer(reader io.ReadCloser, writer io.WriteCloser, permission string, onUpdate func(json.RawMessage), schedules ...bool) *rpcPeer {
	p := &rpcPeer{reader: reader, writer: writer, pending: map[string]chan rpcResult{}, done: make(chan struct{}), onUpdate: onUpdate, permission: permission}
	p.schedules = len(schedules) > 0 && schedules[0]
	go p.read()
	return p
}
func (p *rpcPeer) Close(err error) {
	p.closed.Do(func() {
		if err == nil {
			err = io.EOF
		}
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
		_ = p.reader.Close()
		_ = p.writer.Close()
	})
}
func (p *rpcPeer) write(ctx context.Context, message envelope) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { p.Close(ctx.Err()) })
	defer stop()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = p.writer.Write(append(data, '\n'))
	if err != nil {
		p.Close(err)
	}
	return err
}
func (p *rpcPeer) call(ctx context.Context, method string, params any, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.next++
	id := fmt.Sprint(p.next)
	ch := make(chan rpcResult, 1)
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err = p.write(ctx, envelope{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case reply := <-ch:
		if reply.Err != nil {
			return reply.Err
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	}
}
func (p *rpcPeer) notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return p.write(ctx, envelope{JSONRPC: "2.0", Method: method, Params: raw})
}
func (p *rpcPeer) read() {
	scanner := bufio.NewScanner(p.reader)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		var m envelope
		if json.Unmarshal(scanner.Bytes(), &m) != nil || m.JSONRPC != "2.0" {
			p.Close(errors.New("invalid ACP JSON-RPC frame"))
			return
		}
		if m.Method == "" && len(m.ID) > 0 {
			p.mu.Lock()
			ch := p.pending[string(m.ID)]
			p.mu.Unlock()
			if ch != nil {
				var err error
				if m.Error != nil {
					err = m.Error
				}
				select {
				case ch <- rpcResult{Result: m.Result, Err: err}:
				default:
					p.Close(errors.New("duplicate ACP response"))
					return
				}
			}
			continue
		}
		if m.Method == "session/update" && len(m.ID) == 0 {
			if p.onUpdate != nil {
				p.onUpdate(m.Params)
			}
			continue
		}
		if len(m.ID) > 0 {
			reply := envelope{JSONRPC: "2.0", ID: m.ID}
			if m.Method == "session/request_permission" {
				var params struct {
					Session  string `json:"sessionId"`
					ToolCall struct {
						Title string `json:"title"`
					} `json:"toolCall"`
					Subject struct {
						Type     string `json:"type"`
						ToolCall struct {
							Title string `json:"title"`
						} `json:"toolCall"`
					} `json:"subject"`
					Options []struct {
						ID   string `json:"optionId"`
						Kind string `json:"kind"`
					} `json:"options"`
				}
				_ = json.Unmarshal(m.Params, &params)
				toolName := params.ToolCall.Title
				if params.Subject.Type == "tool_call" {
					toolName = params.Subject.ToolCall.Title
				}
				outcome := map[string]any{"outcome": "cancelled"}
				if p.permission == "allow_once" || toolName == "schedule_read" || p.schedules && toolName == "schedule_manage" {
					for _, option := range params.Options {
						if option.Kind == "allow_once" {
							outcome = map[string]any{"outcome": "selected", "optionId": option.ID}
							break
						}
					}
				}
				if outcome["outcome"] == "cancelled" && p.onUpdate != nil {
					raw, _ := json.Marshal(map[string]any{"sessionId": params.Session, "update": map[string]any{"sessionUpdate": "_buzz_permission_denied"}})
					p.onUpdate(raw)
				}
				reply.Result, _ = json.Marshal(map[string]any{"outcome": outcome})
			} else {
				reply.Error = &RPCError{Code: -32601, Message: "Method not supported by Buzz adapter"}
			}
			// Permission responses never perform Buzz I/O, so an ACP request cannot
			// block behind relay delivery or observer publishing.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.write(ctx, reply)
			cancel()
			if err != nil {
				return
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	p.Close(err)
}

type turnTracker struct {
	mu               sync.Mutex
	running          bool
	permissionDenied bool
	messages         map[string][]map[string]any
	order            []string
	done             chan map[string]any
	updates          func(map[string]any)
}

func (t *turnTracker) apply(u map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	kind, _ := u["sessionUpdate"].(string)
	if kind == "_buzz_permission_denied" {
		t.permissionDenied = true
		return
	}
	if kind == "state_update" {
		state, _ := u["state"].(string)
		if state == "running" {
			t.running = true
		}
		if state == "idle" && t.running {
			select {
			case t.done <- u:
			default:
			}
		}
	}
	if t.running {
		id, _ := u["messageId"].(string)
		if id != "" && (kind == "agent_message_chunk" || kind == "agent_message") {
			if _, exists := t.messages[id]; !exists {
				t.order = append(t.order, id)
				t.messages[id] = nil
			}
			if kind == "agent_message_chunk" {
				if block, ok := u["content"].(map[string]any); ok {
					t.messages[id] = append(t.messages[id], block)
				}
			} else if blocks, ok := u["content"].([]any); ok {
				t.messages[id] = nil
				for _, block := range blocks {
					if value, ok := block.(map[string]any); ok {
						t.messages[id] = append(t.messages[id], value)
					}
				}
			} else if u["content"] == nil {
				t.messages[id] = nil
			}
		}
	}
	if t.updates != nil {
		t.updates(u)
	}
}
func (t *turnTracker) text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out strings.Builder
	for _, id := range t.order {
		for _, block := range t.messages[id] {
			switch block["type"] {
			case "text":
				text, _ := block["text"].(string)
				out.WriteString(text)
			case "resource_link":
				uri, _ := block["uri"].(string)
				name, _ := block["name"].(string)
				if name == "" {
					name = uri
				}
				fmt.Fprintf(&out, "\n[%s](%s)\n", name, uri)
			}
		}
	}
	return strings.TrimSpace(out.String())
}

type PromptError struct {
	Detail    string
	Uncertain bool
}

func (e *PromptError) Error() string { return e.Detail }

type ACPClient struct {
	Config         Config
	mu             sync.Mutex
	peer           *rpcPeer
	cmd            *exec.Cmd
	processDone    chan struct{}
	turns          map[string]*turnTracker
	attached       map[string]bool
	SupportsImages bool
	stderr         io.Writer
}
type bytesSink struct{}

func (bytesSink) Write(p []byte) (int, error) { return len(p), nil } // Child diagnostics may contain provider secrets; do not forward raw stderr.
func NewACP(c Config) *ACPClient {
	return &ACPClient{Config: c, turns: map[string]*turnTracker{}, attached: map[string]bool{}, stderr: bytesSink{}}
}
func (a *ACPClient) Open(ctx context.Context) error {
	a.mu.Lock()
	if a.peer != nil {
		select {
		case <-a.peer.done:
		default:
			a.mu.Unlock()
			return nil
		}
	}
	a.mu.Unlock()
	a.Close()
	cmd := exec.Command(a.Config.Deerflow.Command, a.Config.Deerflow.Args...)
	hideCommand(cmd)
	cmd.Dir = a.Config.Deerflow.Workspace
	cmd.Env = mergeEnv(os.Environ(), a.Config.Deerflow.Env)
	cmd.Stderr = a.stderr
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return fmt.Errorf("cannot start ACP Bridge: %w", err)
	}
	peer := newPeer(stdout, stdin, a.Config.Deerflow.PermissionPolicy, func(raw json.RawMessage) {
		var value struct {
			Session string         `json:"sessionId"`
			Update  map[string]any `json:"update"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		a.mu.Lock()
		turn := a.turns[value.Session]
		a.mu.Unlock()
		if turn != nil {
			turn.apply(value.Update)
		}
	}, a.Config.Deerflow.ScheduleFullAccess && !a.Config.Buzz.AllowAnySender)
	done := make(chan struct{})
	a.mu.Lock()
	a.peer = peer
	a.cmd = cmd
	a.processDone = done
	a.attached = map[string]bool{}
	a.mu.Unlock()
	go func() {
		// Drain stdout before Wait closes its pipe.
		<-peer.done
		err := cmd.Wait()
		if err == nil {
			err = io.EOF
		}
		peer.Close(err)
		close(done)
	}()
	requestCtx, cancel := context.WithTimeout(ctx, seconds(a.Config.Deerflow.Timeout))
	defer cancel()
	var response struct {
		Version      int `json:"protocolVersion"`
		Capabilities struct {
			Session struct {
				Prompt map[string]any `json:"prompt"`
			} `json:"session"`
		} `json:"capabilities"`
	}
	err = peer.call(requestCtx, "initialize", map[string]any{"protocolVersion": 2, "info": map[string]string{"name": "buzz-deerflow-adapter-go", "version": "0.3.0"}, "capabilities": map[string]any{}}, &response)
	if err != nil || response.Version != 2 || response.Capabilities.Session.Prompt == nil {
		a.Close()
		if err != nil {
			return err
		}
		return errors.New("Bridge did not negotiate ACP v2 session support")
	}
	_, a.SupportsImages = response.Capabilities.Session.Prompt["image"]
	return nil
}
func (a *ACPClient) Close() {
	a.mu.Lock()
	peer, cmd, done := a.peer, a.cmd, a.processDone
	a.peer = nil
	a.cmd = nil
	a.processDone = nil
	a.attached = map[string]bool{}
	a.SupportsImages = false
	a.mu.Unlock()
	if peer != nil {
		peer.Close(io.EOF)
	}
	if cmd != nil && done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
}
func (a *ACPClient) AttachOrCreate(ctx context.Context, existing string) (string, error) {
	a.mu.Lock()
	peer := a.peer
	attached := a.attached[existing]
	a.mu.Unlock()
	if peer == nil {
		return "", errors.New("ACP is not connected")
	}
	if attached && existing != "" {
		return existing, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, seconds(a.Config.Deerflow.Timeout))
	defer cancel()
	params := map[string]any{"cwd": a.Config.Deerflow.Workspace, "additionalDirectories": []string{}, "mcpServers": []any{}}
	if existing != "" {
		params["sessionId"] = existing
		err := peer.call(requestCtx, "session/resume", params, nil)
		if err == nil {
			a.mu.Lock()
			a.attached[existing] = true
			a.mu.Unlock()
			return existing, nil
		}
		var rpc *RPCError
		if !errors.As(err, &rpc) || rpc.Code != -32602 || !strings.Contains(strings.ToLower(rpc.Message), "not found") {
			return "", err
		}
		delete(params, "sessionId")
	}
	var result struct {
		ID string `json:"sessionId"`
	}
	if err := peer.call(requestCtx, "session/new", params, &result); err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", errors.New("session/new returned no session ID")
	}
	a.mu.Lock()
	a.attached[result.ID] = true
	a.mu.Unlock()
	return result.ID, nil
}
func (a *ACPClient) Prompt(ctx context.Context, session string, blocks []map[string]any, onUpdate func(map[string]any)) (string, error) {
	a.mu.Lock()
	peer := a.peer
	if peer == nil {
		a.mu.Unlock()
		return "", errors.New("ACP is not connected")
	}
	if a.turns[session] != nil {
		a.mu.Unlock()
		return "", errors.New("ACP session already has a prompt")
	}
	turn := &turnTracker{messages: map[string][]map[string]any{}, done: make(chan map[string]any, 1), updates: onUpdate}
	a.turns[session] = turn
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.turns, session); a.mu.Unlock() }()
	if message, ok := ctx.Value(buzzScheduleMessageKey{}).(Pending); ok {
		if err := a.bindSchedules(ctx, session, message); err != nil {
			return "", &PromptError{Detail: "Trusted Buzz source binding failed; no prompt submitted"}
		}
	}
	promptCtx := ctx
	cancel := func() {}
	if a.Config.Deerflow.PromptTimeout > 0 {
		promptCtx, cancel = context.WithTimeout(ctx, seconds(a.Config.Deerflow.PromptTimeout))
	}
	defer cancel()
	ackCtx, ackCancel := context.WithTimeout(promptCtx, seconds(a.Config.Deerflow.Timeout))
	err := peer.call(ackCtx, "session/prompt", map[string]any{"sessionId": session, "prompt": blocks}, nil)
	ackCancel()
	if err != nil {
		return "", &PromptError{Detail: "prompt submission failed; execution outcome must be checked", Uncertain: true}
	}
	select {
	case idle := <-turn.done:
		turn.mu.Lock()
		denied := turn.permissionDenied
		turn.mu.Unlock()
		if denied {
			return "", &PromptError{Detail: "Tool permission was declined by the Buzz adapter policy"}
		}
		reason, _ := idle["stopReason"].(string)
		if reason == "_deerflow_error" || reason == "cancelled" {
			return "", &PromptError{Detail: "DeerFlow task ended with " + reason}
		}
		return turn.text(), nil
	case <-peer.done:
		return "", &PromptError{Detail: "ACP disconnected after prompt acknowledgement; task was not replayed", Uncertain: true}
	case <-promptCtx.Done():
		cancelCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		_ = peer.notify(cancelCtx, "session/cancel", map[string]string{"sessionId": session})
		stop()
		return "", &PromptError{Detail: "prompt cancelled or timed out; inspect the ACP session", Uncertain: true}
	}
}
