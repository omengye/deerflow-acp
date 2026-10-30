package agent

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

const (
	conversationTurnsMethod  = "_deerflow/history/turns"
	conversationForkMethod   = "_deerflow/history/fork"
	conversationRewindMethod = "_deerflow/history/rewind"
)

func conversationCapabilities() map[string]any {
	return map[string]any{"version": 1, "turnsMethod": conversationTurnsMethod, "forkMethod": conversationForkMethod, "rewindMethod": conversationRewindMethod}
}

func (a *Agent) conversationRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	invalid := func() (any, error) {
		return nil, rpcError(protocol.InvalidParams, "Invalid conversation boundary request")
	}
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	fields := []string{"sessionId"}
	if method != conversationTurnsMethod {
		fields = append(fields, "expectedHeadEventId", "targetEventId", "operationId")
	}
	if _, err := strictObject(raw, fields...); err != nil {
		return invalid()
	}
	var request hr.ConversationBranchRequest
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) {
		return invalid()
	}
	if method == conversationTurnsMethod {
		return a.service.ConversationTurns(ctx, a.owner, request.SessionID)
	}
	if !validID(request.OperationID, 256) || len(request.ExpectedHeadEventID) > 256 || len(request.TargetEventID) > 256 {
		return invalid()
	}
	return a.service.BranchConversation(ctx, a.owner, request, method == conversationRewindMethod)
}
