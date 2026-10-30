package agent

import (
	"context"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const getSessionBudgetMethod = "_deerflow/session/budget/get"
const setSessionBudgetMethod = "_deerflow/session/budget/set"

func sessionBudgetCapabilities() map[string]any {
	return map[string]any{"version": 1, "getMethod": getSessionBudgetMethod, "setMethod": setSessionBudgetMethod}
}

func (a *Agent) sessionBudgetRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	invalid := rpcError(protocol.InvalidParams, "Invalid session budget parameters")
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	keys := []string{"sessionId"}
	if method == setSessionBudgetMethod {
		keys = append(keys, "expectedVersion", "operationId", "limits")
	}
	object, err := strictObject(raw, keys...)
	if err != nil {
		return nil, invalid
	}
	var request harness.SessionBudgetRequest
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) {
		return nil, invalid
	}
	if method == getSessionBudgetMethod {
		return a.service.SessionBudget(ctx, a.owner, request.SessionID)
	}
	if _, err = strictObject(object["limits"], "maxTokens", "maxModelCalls", "maxToolCalls"); err != nil {
		return nil, invalid
	}
	if !validID(request.OperationID, 128) || request.ExpectedVersion < 1 {
		return nil, invalid
	}
	return a.service.SetSessionBudget(ctx, a.owner, request)
}
