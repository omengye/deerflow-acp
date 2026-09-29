// Package agent maps ACP stable v1 to the transport-independent runtime.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Agent struct {
	service      *hr.Service
	owner        string
	peer         *protocol.Peer
	mu           sync.Mutex
	initialized  bool
	capabilities acp.ClientCapabilities
}

func New(service *hr.Service, in io.ReadCloser, out io.WriteCloser) *Agent {
	a := &Agent{service: service, owner: hr.NewID()}
	a.peer = protocol.NewPeer(in, out, a.Handle, protocol.Options{Admit: a.admit})
	return a
}

func (a *Agent) Serve(ctx context.Context) error {
	err := a.peer.Serve(ctx)
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(err, a.service.Disconnect(cleanup, a.owner))
}
func (a *Agent) Close() error { return a.peer.Close() }

func (a *Agent) ready() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.initialized }
func (a *Agent) admit(ctx context.Context, method string, raw json.RawMessage) (context.Context, func(), error) {
	if method != "session/prompt" {
		return ctx, nil, nil
	}
	if !a.ready() {
		return nil, nil, rpcError(protocol.InvalidRequest, "initialize must complete first")
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := decode(raw, &req); err != nil {
		return nil, nil, err
	}
	ctx, release, err := a.service.Admit(ctx, a.owner, req.SessionID)
	return ctx, release, mapError(err)
}

func rpcError(code int, message string) error { return &protocol.Error{Code: code, Message: message} }
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" || raw[0] != '{' {
		return rpcError(protocol.InvalidParams, "params must be an object")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return rpcError(protocol.InvalidParams, "invalid parameters")
	}
	return nil
}
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var p *protocol.Error
	if errors.As(err, &p) {
		return err
	}
	switch {
	case errors.Is(err, harness.ErrBusy), errors.Is(err, harness.ErrAttachedElsewhere):
		return rpcError(protocol.ServerBusy, err.Error())
	case errors.Is(err, harness.ErrNotAttached), errors.Is(err, harness.ErrNotFound), errors.Is(err, harness.ErrInvalidInput):
		return rpcError(protocol.InvalidParams, err.Error())
	}
	return err
}

func (a *Agent) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	result, err := a.handle(ctx, method, raw)
	return result, mapError(err)
}
func (a *Agent) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if protocol.IsNotification(ctx) && method != "session/cancel" {
		return nil, nil
	}
	if method == "initialize" {
		var req acp.InitializeRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.ProtocolVersion < 1 {
			return nil, rpcError(protocol.InvalidParams, "protocolVersion must be positive")
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.initialized {
			return nil, rpcError(protocol.InvalidRequest, "connection is already initialized")
		}
		a.initialized = true
		a.capabilities = req.ClientCapabilities
		return map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "deerflow-go", "title": "DeerFlow Go Harness", "version": "0.1.0-dev"}, "authMethods": []any{}, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": false, "audio": false, "embeddedContext": false}, "mcpCapabilities": map[string]bool{"http": false, "sse": false}, "sessionCapabilities": map[string]any{"list": map[string]any{}, "close": map[string]any{}, "resume": map[string]any{}}}}, nil
	}
	if !a.ready() {
		return nil, rpcError(protocol.InvalidRequest, "initialize must complete first")
	}
	switch method {
	case "session/new":
		var req acp.NewSessionRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if err := req.Validate(); err != nil {
			return nil, rpcError(protocol.InvalidParams, err.Error())
		}
		if err := validateResources(req.AdditionalDirectories, len(req.McpServers)); err != nil {
			return nil, err
		}
		x, err := a.service.NewSession(ctx, a.owner, req.Cwd)
		if err != nil {
			return nil, err
		}
		return sessionResponse(x, true), nil
	case "session/load", "session/resume":
		var req acp.LoadSessionRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if err := req.Validate(); err != nil {
			return nil, rpcError(protocol.InvalidParams, err.Error())
		}
		if err := validateResources(req.AdditionalDirectories, len(req.McpServers)); err != nil {
			return nil, err
		}
		x, err := a.service.Load(ctx, a.owner, string(req.SessionId), req.Cwd, method == "session/load", a.emit)
		if err != nil {
			return nil, err
		}
		return sessionResponse(x, false), nil
	case "session/list":
		var req acp.ListSessionsRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		cwd, cursor := "", ""
		if req.Cwd != nil {
			var err error
			cwd, err = session.NormalizeWorkspace(*req.Cwd)
			if err != nil {
				return nil, err
			}
		}
		if req.Cursor != nil {
			cursor = *req.Cursor
		}
		xs, err := a.service.Store.List(ctx, cwd, cursor, 51)
		if err != nil {
			return nil, err
		}
		var next string
		if len(xs) > 50 {
			xs = xs[:50]
			next = xs[49].ID
		}
		items := make([]any, 0, len(xs))
		for _, x := range xs {
			items = append(items, map[string]any{"sessionId": x.ID, "cwd": x.CWD, "title": x.Title, "updatedAt": x.UpdatedAt.Format(time.RFC3339Nano)})
		}
		result := map[string]any{"sessions": items}
		if next != "" {
			result["nextCursor"] = next
		}
		return result, nil
	case "session/close":
		var req acp.CloseSessionRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return map[string]any{}, a.service.Coordinator.Detach(ctx, string(req.SessionId), a.owner)
	case "session/set_mode":
		var req acp.SetSessionModeRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if err := a.service.SetMode(ctx, a.owner, string(req.SessionId), string(req.ModeId)); err != nil {
			return nil, err
		}
		if err := a.update(ctx, string(req.SessionId), map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": req.ModeId}); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	case "session/prompt":
		var req struct {
			SessionID string            `json:"sessionId"`
			Prompt    []harness.Content `json:"prompt"`
		}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		result, err := a.service.Run(ctx, a.owner, req.SessionID, req.Prompt, a.emit, a.permission)
		if err != nil {
			return nil, err
		}
		return acp.PromptResponse{StopReason: acp.StopReason(result.StopReason)}, nil
	case "session/cancel":
		if !protocol.IsNotification(ctx) {
			return nil, rpcError(protocol.InvalidRequest, "session/cancel is a notification")
		}
		var req acp.CancelNotification
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return nil, a.service.Coordinator.Cancel(string(req.SessionId), a.owner)
	default:
		return nil, rpcError(protocol.MethodNotFound, "Method not found")
	}
}

func validateResources(additional []string, mcpCount int) error {
	if len(additional) > 0 {
		return rpcError(protocol.InvalidParams, "additionalDirectories is not supported")
	}
	// Explicitly incomplete until the scoped MCP manager is installed. Capability
	// completeness is not claimed by this development build.
	if mcpCount > 0 {
		return rpcError(protocol.InvalidParams, "client MCP is not yet configured in this development build")
	}
	return nil
}
func sessionResponse(x harness.Session, includeID bool) map[string]any {
	r := map[string]any{"modes": map[string]any{"currentModeId": x.Mode, "availableModes": []any{map[string]any{"id": "default", "name": "Default", "description": "Execute tasks with approved tools."}, map[string]any{"id": "plan", "name": "Plan", "description": "Inspect and plan using read-only workspace tools."}}}}
	if includeID {
		r["sessionId"] = x.ID
	}
	return r
}
func (a *Agent) update(ctx context.Context, id string, update any) error {
	return a.connectionError(ctx, a.peer.Notify(ctx, "session/update", map[string]any{"sessionId": id, "update": update}))
}

func (a *Agent) connectionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-a.peer.Done():
		return context.Canceled
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (a *Agent) emit(ctx context.Context, e harness.RunEvent) error {
	var update map[string]any
	switch e.Kind {
	case "user_message":
		for _, c := range e.Content {
			if err := a.update(ctx, e.SessionID, map[string]any{"sessionUpdate": "user_message_chunk", "content": c}); err != nil {
				return err
			}
		}
		return nil
	case "text_delta", "reasoning_delta":
		kind := "agent_message_chunk"
		if e.Kind == "reasoning_delta" {
			kind = "agent_thought_chunk"
		}
		update = map[string]any{"sessionUpdate": kind, "content": harness.Content{Type: "text", Text: e.Text}}
	case "tool_start":
		update = map[string]any{"sessionUpdate": "tool_call", "toolCallId": e.ToolCallID, "title": e.ToolName, "kind": "other", "status": e.Status, "rawInput": e.Arguments}
	case "tool_update", "tool_end":
		update = map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": e.ToolCallID, "status": e.Status}
		var content []any
		for _, c := range e.Content {
			content = append(content, map[string]any{"type": "content", "content": c})
		}
		if e.Text != "" {
			content = append(content, map[string]any{"type": "content", "content": harness.Content{Type: "text", Text: e.Text}})
		}
		if len(content) > 0 {
			update["content"] = content
		}
	default:
		return nil
	}
	return a.update(ctx, e.SessionID, update)
}

func (a *Agent) permission(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
	options := []map[string]string{{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"}, {"optionId": "allow_always", "name": "Allow identical calls in this session", "kind": "allow_always"}, {"optionId": "reject_once", "name": "Reject once", "kind": "reject_once"}, {"optionId": "reject_always", "name": "Reject identical calls in this session", "kind": "reject_always"}}
	request := map[string]any{"sessionId": p.SessionID, "toolCall": map[string]any{"toolCallId": p.ToolCallID, "title": p.ToolName, "status": "pending", "rawInput": p.Arguments}, "options": options}
	var response struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := a.peer.Call(ctx, "session/request_permission", request, &response); err != nil {
		return harness.PermissionCancelled, a.connectionError(ctx, err)
	}
	if response.Outcome.Outcome == "cancelled" {
		return harness.PermissionCancelled, nil
	}
	if response.Outcome.Outcome != "selected" {
		return harness.RejectOnce, fmt.Errorf("invalid permission outcome")
	}
	decision := harness.PermissionDecision(response.Outcome.OptionID)
	switch decision {
	case harness.AllowOnce, harness.AllowAlways, harness.RejectOnce, harness.RejectAlways:
		return decision, nil
	default:
		return harness.RejectOnce, fmt.Errorf("unknown permission option")
	}
}
