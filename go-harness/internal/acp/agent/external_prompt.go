package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	pendingExternalPromptMethod = "_deerflow/external_prompt/pending"
	ackExternalPromptMethod     = "_deerflow/external_prompt/acknowledge"
)

func externalPromptCapabilities() map[string]any {
	return map[string]any{"version": 1, "pendingMethod": pendingExternalPromptMethod, "acknowledgeMethod": ackExternalPromptMethod}
}

func (a *Agent) externalPromptRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if a.service.ExternalPromptPending == nil || a.service.ExternalPromptAcknowledge == nil {
		return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "Method not found"}
	}
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return nil, rpcError(protocol.InvalidParams, "Invalid external prompt parameters")
	}
	keys := []string{"sessionId", "agent"}
	if method == ackExternalPromptMethod {
		keys = append(keys, "promptId")
	}
	fields, err := strictObject(raw, keys...)
	if err != nil {
		return nil, rpcError(protocol.InvalidParams, "Invalid external prompt parameters")
	}
	var sessionID, agentName, promptID string
	if json.Unmarshal(fields["sessionId"], &sessionID) != nil || json.Unmarshal(fields["agent"], &agentName) != nil || !validID(sessionID, 256) || !validID(agentName, 64) {
		return nil, rpcError(protocol.InvalidParams, "Invalid external prompt identity")
	}
	if method == pendingExternalPromptMethod {
		pending, err := a.service.ExternalPromptPending(ctx, a.owner, sessionID, agentName)
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"pending": nil}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"pending": pending}, nil
	}
	if json.Unmarshal(fields["promptId"], &promptID) != nil || !validID(promptID, 128) {
		return nil, rpcError(protocol.InvalidParams, "Invalid external prompt ID")
	}
	if err := a.service.ExternalPromptAcknowledge(ctx, a.owner, sessionID, agentName, promptID); err != nil {
		return nil, err
	}
	return map[string]any{"acknowledged": promptID}, nil
}
