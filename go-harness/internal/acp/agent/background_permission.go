package agent

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const getBackgroundPermissionMethod = "_deerflow/tasks/permission/get"

func (a *Agent) backgroundPermissionRequest(ctx context.Context, raw json.RawMessage) (result any, returnErr error) {
	defer func() { returnErr = backgroundError(returnErr) }()
	reader, ok := a.service.Background.(harness.BackgroundPermissionReader)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Background permission preview is unavailable")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid background permission preview parameters")
	if len(raw) > 8192 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	if _, err := strictObject(raw, "sessionId", "taskId", "interactionId", "taskVersion", "intentId"); err != nil {
		return nil, invalid
	}
	var req struct {
		SessionID string `json:"sessionId"`
		harness.BackgroundPermissionQuery
	}
	if json.Unmarshal(raw, &req) != nil || !validID(req.SessionID, 256) || !validID(req.TaskID, 256) || !validID(req.InteractionID, 256) || !validID(req.IntentID, 256) || req.TaskVersion < 1 {
		return nil, invalid
	}
	return reader.BackgroundPermission(ctx, harness.TaskActor{OwnerID: a.owner, SessionID: req.SessionID}, req.BackgroundPermissionQuery)
}
