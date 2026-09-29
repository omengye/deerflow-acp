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
	subagents    map[subagentKey]bool
	contentIDs   map[contentKey]contentState
}

type subagentKey struct{ sessionID, runID, callID string }
type contentKey struct{ sessionID, role string }
type contentState struct{ runID, messageID string }

func New(service *hr.Service, in io.ReadCloser, out io.WriteCloser) *Agent {
	a := &Agent{service: service, owner: hr.NewID(), subagents: make(map[subagentKey]bool), contentIDs: make(map[contentKey]contentState)}
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
	if method != "session/prompt" && method != resumeExecutionMethod && method != processBackgroundNotificationMethod {
		return ctx, nil, nil
	}
	ctx = hr.WithTransportCancellation(ctx)
	if !a.ready() {
		return nil, nil, rpcError(protocol.InvalidRequest, "initialize must complete first")
	}
	if method == resumeExecutionMethod {
		if _, err := decodeExecutionRequest(method, raw); err != nil {
			return nil, nil, err
		}
	}
	if method == processBackgroundNotificationMethod {
		if _, err := a.notificationProcessor(); err != nil {
			return nil, nil, err
		}
		if _, err := decodeBackgroundRequest(method, raw); err != nil {
			return nil, nil, err
		}
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
	case errors.Is(err, harness.ErrExecutionWaitingInput):
		return executionRecoveryError(executionWaitingCode)
	case errors.Is(err, harness.ErrExecutionConflict):
		return executionRecoveryError(executionConflictCode)
	case errors.Is(err, harness.ErrExecutionUnresumable):
		return executionRecoveryError(executionUnresumableCode)
	case errors.Is(err, harness.ErrReconciliationRequired):
		return receiptRecoveryError(false)
	case errors.Is(err, harness.ErrReceiptConflict):
		return receiptRecoveryError(true)
	case errors.Is(err, harness.ErrBusy):
		return rpcError(protocol.ServerBusy, "Session is busy")
	case errors.Is(err, harness.ErrAttachedElsewhere):
		return rpcError(protocol.ServerBusy, "Session is attached to another connection")
	case errors.Is(err, harness.ErrNotAttached):
		return rpcError(protocol.InvalidParams, "Session is not attached to this connection")
	case errors.Is(err, harness.ErrNotFound):
		return rpcError(protocol.InvalidParams, "Requested session or tool receipt was not found")
	case errors.Is(err, harness.ErrInvalidInput):
		return rpcError(protocol.InvalidParams, "Invalid request parameters")
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
		httpMCP, sseMCP := a.service.MCPCapabilities()
		response := map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "deerflow-go", "title": "DeerFlow Go Harness", "version": "0.1.0-dev"}, "authMethods": []any{}, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": a.service.ImageInputEnabled(), "audio": false, "embeddedContext": false}, "mcpCapabilities": map[string]bool{"http": httpMCP, "sse": sseMCP}, "sessionCapabilities": map[string]any{"list": map[string]any{}, "close": map[string]any{}, "resume": map[string]any{}, "delete": map[string]any{}}}, "_meta": map[string]any{"deerflow": map[string]any{"toolReceipts": receiptCapabilities(), "history": map[string]any{"version": 1, "listMethod": historyListMethod}, "artifacts": map[string]any{"version": 1, "listMethod": listArtifactsMethod}}}}
		if a.service.Memory != nil {
			response["_meta"].(map[string]any)["deerflow"].(map[string]any)["memory"] = memoryCapabilities(a.service.MemoryUserID != "")
		}
		if a.service.DurableExecutionsEnabled() {
			response["_meta"].(map[string]any)["deerflow"].(map[string]any)["executions"] = executionCapabilities()
		}
		if a.service.Background != nil {
			response["_meta"].(map[string]any)["deerflow"].(map[string]any)["background"] = backgroundCapabilities(a.service.Background)
		}
		return response, nil
	}
	if !a.ready() {
		return nil, rpcError(protocol.InvalidRequest, "initialize must complete first")
	}
	switch method {
	case processBackgroundNotificationMethod:
		return a.processBackgroundNotification(ctx, raw)
	case getBackgroundPermissionMethod:
		return a.backgroundPermissionRequest(ctx, raw)
	case listBackgroundTasksMethod, getBackgroundTaskMethod, waitBackgroundTaskMethod, cancelBackgroundTaskMethod, resumeBackgroundTaskMethod, approveBackgroundTaskMethod, listBackgroundNotificationsMethod, ackBackgroundNotificationMethod:
		return a.backgroundRequest(ctx, method, raw)
	case getExecutionMethod, resumeExecutionMethod, cancelExecutionMethod:
		return a.executionRequest(ctx, method, raw)
	case historyListMethod:
		return a.historyRequest(ctx, raw)
	case listArtifactsMethod:
		return a.artifactRequest(ctx, raw)
	case listReceiptsMethod, reconcileReceiptMethod:
		return a.receiptRequest(ctx, method, raw)
	case listMemoryMethod, searchMemoryMethod, getMemoryMethod, createMemoryMethod, replaceMemoryMethod, deleteMemoryMethod, clearMemoryMethod, flushMemoryMethod:
		return a.memoryRequest(ctx, method, raw)
	case "session/new":
		var req acp.NewSessionRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if err := req.Validate(); err != nil {
			return nil, rpcError(protocol.InvalidParams, err.Error())
		}
		if err := validateResources(req.AdditionalDirectories); err != nil {
			return nil, err
		}
		servers, err := a.mcpServers(raw, false)
		if err != nil {
			return nil, err
		}
		x, err := a.service.NewSession(ctx, a.owner, req.Cwd, servers...)
		if err != nil {
			return nil, err
		}
		return a.sessionResponse(x, true), nil
	case "session/load", "session/resume":
		var sessionID, cwd string
		var additional []string
		if method == "session/load" {
			var req acp.LoadSessionRequest
			if err := decode(raw, &req); err != nil {
				return nil, err
			}
			if err := req.Validate(); err != nil {
				return nil, rpcError(protocol.InvalidParams, err.Error())
			}
			sessionID, cwd, additional = string(req.SessionId), req.Cwd, req.AdditionalDirectories
		} else {
			var req acp.ResumeSessionRequest
			if err := decode(raw, &req); err != nil {
				return nil, err
			}
			if err := req.Validate(); err != nil {
				return nil, rpcError(protocol.InvalidParams, err.Error())
			}
			sessionID, cwd, additional = string(req.SessionId), req.Cwd, req.AdditionalDirectories
		}
		if sessionID == "" {
			return nil, rpcError(protocol.InvalidParams, "sessionId is required")
		}
		if err := validateResources(additional); err != nil {
			return nil, err
		}
		servers, err := a.mcpServers(raw, method == "session/resume")
		if err != nil {
			return nil, err
		}
		x, err := a.service.Load(ctx, a.owner, sessionID, cwd, method == "session/load", a.emit, servers...)
		if err != nil {
			return nil, err
		}
		return a.sessionResponse(x, false), nil
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
		if err := a.service.CloseSession(ctx, a.owner, string(req.SessionId)); err != nil {
			return nil, err
		}
		a.clearSubagents(string(req.SessionId))
		return map[string]any{}, nil
	case "session/delete":
		var req acp.UnstableDeleteSessionRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		id := string(req.SessionId)
		if !validID(id, 256) {
			return nil, rpcError(protocol.InvalidParams, "invalid sessionId")
		}
		if _, err := a.service.DeleteAttachedSession(ctx, a.owner, id); err != nil {
			return nil, err
		}
		a.clearSubagents(id)
		return map[string]any{}, nil
	case "session/set_config_option":
		var req struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
			Type      string `json:"type"`
		}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.SessionID == "" || req.ConfigID == "" || req.Value == "" || (req.Type != "" && req.Type != "select") {
			return nil, rpcError(protocol.InvalidParams, "invalid select configuration request")
		}
		options, err := a.service.SetConfigOption(ctx, a.owner, req.SessionID, req.ConfigID, req.Value)
		if err != nil {
			return nil, err
		}
		if err = a.update(ctx, req.SessionID, map[string]any{"sessionUpdate": "config_option_update", "configOptions": options}); err != nil {
			return nil, err
		}
		return map[string]any{"configOptions": options}, nil
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
		req, err := decodePrompt(raw)
		if err != nil {
			return nil, err
		}
		return a.runResponse(func(emit harness.EventHandler) (harness.RunResult, error) {
			return a.service.Run(hr.WithTransportCancellation(ctx), a.owner, req.SessionID, req.Prompt, emit, a.permission)
		})
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

func validateResources(additional []string) error {
	if len(additional) > 0 {
		return rpcError(protocol.InvalidParams, "additionalDirectories is not supported")
	}
	return nil
}
func (a *Agent) sessionResponse(x harness.Session, includeID bool) map[string]any {
	r := map[string]any{"modes": map[string]any{"currentModeId": x.Mode, "availableModes": []any{map[string]any{"id": "default", "name": "Default", "description": "Execute tasks with approved tools."}, map[string]any{"id": "plan", "name": "Plan", "description": "Inspect and plan using read-only workspace tools."}}}}
	r["configOptions"] = a.service.ConfigOptions(x)
	if includeID {
		r["sessionId"] = x.ID
	}
	return r
}
func (a *Agent) update(ctx context.Context, id string, update any) error {
	return a.connectionError(ctx, a.peer.Notify(ctx, "session/update", map[string]any{"sessionId": id, "update": update}))
}

func (a *Agent) updateAsync(ctx context.Context, id string, update any) error {
	return a.connectionError(ctx, a.peer.NotifyAsync(ctx, "session/update", map[string]any{"sessionId": id, "update": update}))
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
	case "subagent_start", "subagent_resumed", "subagent_suspended", "subagent_end":
		return a.emitSubagent(ctx, e)
	case "user_message":
		for _, c := range e.Content {
			wire, err := a.wireContent(ctx, e.SessionID, c)
			if err != nil {
				return err
			}
			if err := a.updateAsync(ctx, e.SessionID, map[string]any{"sessionUpdate": "user_message_chunk", "messageId": contentMessageID(e, "user"), "content": wire}); err != nil {
				return err
			}
		}
		return nil
	case "text_delta", "reasoning_delta":
		kind := "agent_message_chunk"
		if e.Kind == "reasoning_delta" {
			kind = "agent_thought_chunk"
		}
		role := "assistant"
		if e.Kind == "reasoning_delta" {
			role = "thought"
		}
		update = map[string]any{"sessionUpdate": kind, "messageId": a.streamMessageID(e, role), "content": harness.Content{Type: "text", Text: e.Text}}
	case "context_usage":
		if e.ContextUsage == nil || e.ContextUsage.Size <= 0 || e.ContextUsage.Used < 0 {
			return nil
		}
		update = map[string]any{"sessionUpdate": "usage_update", "size": e.ContextUsage.Size, "used": e.ContextUsage.Used}
	case "plan_update":
		entries := make([]acp.PlanEntry, 0, len(e.Plan))
		for _, item := range e.Plan {
			if item.Content == "" {
				continue
			}
			status := acp.PlanEntryStatus(item.Status)
			if status != acp.PlanEntryStatusInProgress && status != acp.PlanEntryStatusCompleted {
				status = acp.PlanEntryStatusPending
			}
			priority := acp.PlanEntryPriority(item.Priority)
			if priority != acp.PlanEntryPriorityHigh && priority != acp.PlanEntryPriorityLow {
				priority = acp.PlanEntryPriorityMedium
			}
			entries = append(entries, acp.PlanEntry{Content: item.Content, Status: status, Priority: priority})
		}
		update = map[string]any{"sessionUpdate": "plan", "entries": entries}
	case "budget_exhausted":
		update = map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": a.streamMessageID(e, "assistant"), "content": harness.Content{Type: "text", Text: e.Text}, "_meta": map[string]any{"deerflow": map[string]any{"event": "budget_exhausted"}}}
	case "tool_start":
		a.resetContentStream(e)
		update = map[string]any{"sessionUpdate": "tool_call", "toolCallId": e.ToolCallID, "title": e.ToolName, "kind": "other", "status": e.Status, "rawInput": e.Arguments}
	case "tool_execute", "tool_update", "tool_end", "tool_reconciled":
		update = map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": e.ToolCallID, "status": e.Status}
		if e.Kind == "tool_reconciled" && e.Status != "completed" {
			update["status"] = "failed"
		}
		var content []any
		for _, c := range e.Content {
			if e.Kind == "tool_end" && c.Asset != nil && c.Asset.Kind == harness.AssetArtifact {
				continue
			}
			wire, err := a.wireContent(ctx, e.SessionID, c)
			if err != nil {
				return err
			}
			content = append(content, map[string]any{"type": "content", "content": wire})
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
	if e.Receipt != nil {
		update["_meta"] = map[string]any{"deerflow": map[string]any{"receipt": map[string]any{"runId": e.Receipt.RunID, "toolCallId": e.Receipt.ToolCallID, "state": e.Receipt.State, "version": e.Receipt.Version, "review": e.Receipt.Review}}}
	}
	if err := a.updateAsync(ctx, e.SessionID, update); err != nil {
		return err
	}
	if e.Kind == "tool_end" {
		for _, c := range e.Content {
			if c.Asset == nil || c.Asset.Kind != harness.AssetArtifact {
				continue
			}
			wire, err := a.wireContent(ctx, e.SessionID, c)
			if err != nil {
				return err
			}
			if err := a.updateAsync(ctx, e.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": a.streamMessageID(e, "assistant"), "content": wire}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ACP v1 permits an omitted messageId, but the v2 Bridge requires one to
// preserve streamed and replayed content.
func contentMessageID(e harness.RunEvent, role string) string {
	if e.RunID != "" {
		return e.RunID + "/" + role
	}
	return fmt.Sprintf("%s/%d/%s", e.SessionID, e.Sequence, role)
}

// A tool call separates assistant messages within a run. The first persisted
// event sequence after that boundary identifies the new message, so live
// streaming and a full session/load replay derive the same IDs.
func (a *Agent) streamMessageID(e harness.RunEvent, role string) string {
	key := contentKey{e.SessionID, role}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.contentIDs[key]
	if current.runID != e.RunID || current.messageID == "" {
		current = contentState{runID: e.RunID, messageID: fmt.Sprintf("%s/%d", contentMessageID(e, role), e.Sequence)}
		a.contentIDs[key] = current
	}
	return current.messageID
}

func (a *Agent) resetContentStream(e harness.RunEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, role := range []string{"assistant", "thought"} {
		delete(a.contentIDs, contentKey{e.SessionID, role})
	}
}

func (a *Agent) emitSubagent(ctx context.Context, e harness.RunEvent) error {
	if e.SessionID == "" || e.RunID == "" || e.ToolCallID == "" {
		return nil
	}
	id := "subagent:" + e.RunID + ":" + e.ToolCallID
	key := subagentKey{sessionID: e.SessionID, runID: e.RunID, callID: e.ToolCallID}
	a.mu.Lock()
	started := a.subagents[key]
	if e.Kind == "subagent_end" {
		delete(a.subagents, key)
	} else {
		a.subagents[key] = true
	}
	a.mu.Unlock()
	if e.Kind == "subagent_start" || !started {
		title := e.ToolName
		if title == "" {
			title = "Subagent"
		}
		if err := a.updateAsync(ctx, e.SessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": title, "kind": "think", "status": "in_progress"}); err != nil {
			return err
		}
		if e.Kind == "subagent_start" {
			return nil
		}
	}
	status := "in_progress"
	if e.Kind == "subagent_end" {
		status = e.Status
		if status != "completed" {
			status = "failed"
		}
	}
	update := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status}
	if e.Kind == "subagent_suspended" {
		update["_meta"] = map[string]any{"deerflow": map[string]any{"state": "waiting_input"}}
	} else if e.Kind == "subagent_resumed" {
		update["_meta"] = map[string]any{"deerflow": map[string]any{"state": "running"}}
	}
	if len(e.Content) > 0 {
		content := make([]any, 0, len(e.Content))
		for _, item := range e.Content {
			wire, err := a.wireContent(ctx, e.SessionID, item)
			if err != nil {
				return err
			}
			content = append(content, map[string]any{"type": "content", "content": wire})
		}
		update["content"] = content
	}
	return a.updateAsync(ctx, e.SessionID, update)
}

func (a *Agent) clearSubagents(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.subagents {
		if key.sessionID == sessionID {
			delete(a.subagents, key)
		}
	}
	for key := range a.contentIDs {
		if key.sessionID == sessionID {
			delete(a.contentIDs, key)
		}
	}
}

func (a *Agent) permission(ctx context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
	options := []map[string]string{{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"}, {"optionId": "allow_always", "name": "Allow identical calls in this session", "kind": "allow_always"}, {"optionId": "reject_once", "name": "Reject once", "kind": "reject_once"}, {"optionId": "reject_always", "name": "Reject identical calls in this session", "kind": "reject_always"}}
	request := map[string]any{"sessionId": p.SessionID, "toolCall": map[string]any{"toolCallId": p.ToolCallID, "title": p.ToolName, "status": "pending", "rawInput": p.Arguments}, "options": options, "_meta": map[string]any{"deerflow": map[string]any{"approvalId": p.ID, "configVersion": p.ConfigVersion}}}
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
