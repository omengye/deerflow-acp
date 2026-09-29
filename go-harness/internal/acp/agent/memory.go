package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	listMemoryMethod    = "_deerflow/memory/list"
	searchMemoryMethod  = "_deerflow/memory/search"
	getMemoryMethod     = "_deerflow/memory/get"
	createMemoryMethod  = "_deerflow/memory/create"
	replaceMemoryMethod = "_deerflow/memory/replace"
	deleteMemoryMethod  = "_deerflow/memory/delete"
	clearMemoryMethod   = "_deerflow/memory/clear"
	memoryConflictCode  = -32016
)

func memoryCapabilities(user bool) map[string]any {
	scopes := []string{"session", "workspace"}
	if user {
		scopes = append(scopes, "user")
	}
	return map[string]any{"version": 1, "scopes": scopes, "listMethod": listMemoryMethod, "searchMethod": searchMemoryMethod, "getMethod": getMemoryMethod, "createMethod": createMemoryMethod, "replaceMethod": replaceMemoryMethod, "deleteMethod": deleteMemoryMethod, "clearMethod": clearMemoryMethod}
}

type memoryRequest struct {
	SessionID             string                  `json:"sessionId"`
	Scope                 harness.MemoryScope     `json:"scope"`
	Cursor                string                  `json:"cursor"`
	Limit                 int                     `json:"limit"`
	Query                 string                  `json:"query"`
	FactID                string                  `json:"factId"`
	ExpectedRevision      int64                   `json:"expectedRevision"`
	ExpectedScopeRevision int64                   `json:"expectedScopeRevision"`
	Fact                  harness.MemoryCandidate `json:"fact"`
}

func decodeMemoryRequest(method string, raw json.RawMessage, userEnabled bool) (memoryRequest, error) {
	var req memoryRequest
	invalid := rpcError(protocol.InvalidParams, "Invalid memory parameters")
	if len(raw) > 20*1024 || rejectDuplicateKeys(raw) != nil {
		return req, invalid
	}
	keys := []string{"sessionId", "scope"}
	switch method {
	case listMemoryMethod:
		keys = append(keys, "cursor", "limit")
	case searchMemoryMethod:
		keys = append(keys, "query", "limit")
	case getMemoryMethod:
		keys = append(keys, "factId")
	case createMemoryMethod:
		keys = append(keys, "fact")
	case replaceMemoryMethod:
		keys = append(keys, "factId", "expectedRevision", "fact")
	case deleteMemoryMethod:
		keys = append(keys, "factId", "expectedRevision")
	case clearMemoryMethod:
		keys = append(keys, "expectedScopeRevision")
	default:
		return req, rpcError(protocol.MethodNotFound, "Method not found")
	}
	fields, err := strictObject(raw, keys...)
	if err != nil || json.Unmarshal(raw, &req) != nil || !validID(req.SessionID, 256) {
		return req, invalid
	}
	if _, ok := fields["scope"]; !ok {
		return req, invalid
	}
	if req.Scope != harness.MemorySession && req.Scope != harness.MemoryWorkspace && !(userEnabled && req.Scope == harness.MemoryUser) {
		return req, invalid
	}
	if _, ok := fields["fact"]; ok {
		parts, err := strictObject(fields["fact"], "content", "category", "confidence")
		if err != nil || len(parts) != 3 || req.Fact.Content == "" || req.Fact.Category == "" || req.Fact.Confidence < 0 || req.Fact.Confidence > 1 {
			return req, invalid
		}
	}
	switch method {
	case listMemoryMethod:
		if len(req.Cursor) > 256 || req.Limit < 0 || req.Limit > 100 {
			return req, invalid
		}
	case searchMemoryMethod:
		if _, ok := fields["query"]; !ok || req.Query == "" || len(req.Query) > 1024 || req.Limit < 0 || req.Limit > 100 {
			return req, invalid
		}
		if req.Limit == 0 {
			req.Limit = 20
		}
	case getMemoryMethod, replaceMemoryMethod, deleteMemoryMethod:
		if !validID(req.FactID, 256) {
			return req, invalid
		}
	}
	if method == createMemoryMethod || method == replaceMemoryMethod {
		if _, ok := fields["fact"]; !ok {
			return req, invalid
		}
	}
	if method == replaceMemoryMethod || method == deleteMemoryMethod {
		if _, ok := fields["expectedRevision"]; !ok || req.ExpectedRevision < 1 {
			return req, invalid
		}
	}
	if method == clearMemoryMethod {
		if _, ok := fields["expectedScopeRevision"]; !ok || req.ExpectedScopeRevision < 0 {
			return req, invalid
		}
	}
	return req, nil
}

func (a *Agent) memoryRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if a.service.Memory == nil {
		return nil, rpcError(protocol.MethodNotFound, "Memory is not available")
	}
	req, err := decodeMemoryRequest(method, raw, a.service.MemoryUserID != "")
	if err != nil {
		return nil, err
	}
	var result any
	switch method {
	case listMemoryMethod:
		result, err = a.service.MemoryFacts(ctx, a.owner, req.SessionID, req.Scope, req.Cursor, req.Limit)
	case searchMemoryMethod:
		var facts []harness.MemoryFact
		facts, err = a.service.SearchMemory(ctx, a.owner, req.SessionID, req.Scope, req.Query, req.Limit)
		result = map[string]any{"facts": facts}
	case getMemoryMethod:
		result, err = a.service.MemoryFact(ctx, a.owner, req.SessionID, req.Scope, req.FactID)
	case createMemoryMethod:
		result, err = a.service.CreateMemoryFact(ctx, a.owner, req.SessionID, req.Scope, req.Fact)
	case replaceMemoryMethod:
		result, err = a.service.ReplaceMemoryFact(ctx, a.owner, req.SessionID, req.Scope, req.FactID, req.ExpectedRevision, req.Fact)
	case deleteMemoryMethod:
		err = a.service.DeleteMemoryFact(ctx, a.owner, req.SessionID, req.Scope, req.FactID, req.ExpectedRevision)
		result = map[string]any{}
	case clearMemoryMethod:
		var count int
		count, err = a.service.ClearMemory(ctx, a.owner, req.SessionID, req.Scope, req.ExpectedScopeRevision)
		result = map[string]any{"cleared": count}
	}
	if errors.Is(err, harness.ErrExecutionConflict) {
		return nil, &protocol.Error{Code: memoryConflictCode, Message: "Memory revision changed; list the scope and retry with its current version", Data: map[string]any{"listMethod": listMemoryMethod}}
	}
	return result, err
}
