package agent

import (
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const historyListMethod = "_deerflow/history/list"

func (a *Agent) historyRequest(ctx context.Context, raw json.RawMessage) (any, error) {
	invalid := func() (any, error) { return nil, rpcError(protocol.InvalidParams, "Invalid history page parameters") }
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	if _, err := strictObject(raw, "sessionId", "cursor", "limit"); err != nil {
		return invalid()
	}
	var request struct {
		SessionID string `json:"sessionId"`
		Cursor    string `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) || len(request.Cursor) > 1024 || request.Limit < 0 || request.Limit > 500 {
		return invalid()
	}
	return a.service.HistoryPage(ctx, a.owner, request.SessionID, request.Cursor, request.Limit)
}
