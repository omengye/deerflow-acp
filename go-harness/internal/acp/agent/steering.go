package agent

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const submitSteeringMethod = "_deerflow/steering/submit"
const listSteeringMethod = "_deerflow/steering/list"
const updatedSteeringMethod = "_deerflow/steering/updated"

func steeringCapabilities() map[string]any {
	return map[string]any{"version": 1, "submitMethod": submitSteeringMethod, "listMethod": listSteeringMethod, "updatedNotification": updatedSteeringMethod, "safePoint": "next_model_request", "scope": "current_root_run"}
}

func (a *Agent) notifySteeringStates(ctx context.Context, sessionID, runID string) {
	if records, err := a.service.ListSteering(ctx, a.owner, sessionID, runID); err == nil {
		for _, record := range records.Items {
			_ = a.peer.NotifyAsync(ctx, updatedSteeringMethod, record)
		}
	}
}

func (a *Agent) steeringRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if !a.service.SteeringEnabled() {
		return nil, rpcError(protocol.MethodNotFound, "Current execution does not support steering")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid steering parameters")
	if len(raw) > 32768 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	keys := []string{"sessionId", "runId"}
	if method == submitSteeringMethod {
		keys = append(keys, "requestId", "expectedVersion", "text")
	}
	if _, err := strictObject(raw, keys...); err != nil {
		return nil, invalid
	}
	var request struct {
		SessionID string `json:"sessionId"`
		harness.SteeringRequest
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) || request.RunID != "" && !validID(request.RunID, 256) {
		return nil, invalid
	}
	if method == listSteeringMethod {
		return a.service.ListSteering(ctx, a.owner, request.SessionID, request.RunID)
	}
	if method != submitSteeringMethod || !validID(request.RunID, 256) || !validID(request.RequestID, 128) || request.ExpectedVersion < 1 {
		return nil, invalid
	}
	record, err := a.service.SubmitSteering(ctx, a.owner, request.SessionID, request.SteeringRequest)
	if err == nil {
		_ = a.peer.NotifyAsync(ctx, updatedSteeringMethod, record)
	}
	return record, err
}
