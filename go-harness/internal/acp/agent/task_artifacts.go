package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const taskArtifactReadMethod = "_deerflow/tasks/tree/artifacts/read"
const taskArtifactListMethod = "_deerflow/tasks/tree/artifacts/list"

func (a *Agent) taskArtifactListRequest(ctx context.Context, raw json.RawMessage) (result any, returnErr error) {
	defer func() { returnErr = backgroundError(returnErr) }()
	lister, ok := a.service.Background.(harness.TaskArtifactLister)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Task artifact listing is unavailable")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid task artifact parameters")
	if len(raw) > 32*1024 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	if _, err := strictObject(raw, "sessionId", "rootId", "nodeId", "cursor", "limit"); err != nil {
		return nil, invalid
	}
	var request struct {
		SessionID string `json:"sessionId"`
		RootID    string `json:"rootId"`
		NodeID    string `json:"nodeId"`
		Cursor    string `json:"cursor"`
		Limit     int    `json:"limit"`
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) || !validTreeID(request.RootID) || !validTreeID(request.NodeID) || len(request.Cursor) > 16384 || request.Limit < 0 || request.Limit > 100 {
		return nil, invalid
	}
	page, err := lister.TaskArtifacts(ctx, harness.TaskActor{OwnerID: a.owner, SessionID: request.SessionID}, harness.TaskArtifactQuery{RootID: request.RootID, NodeID: request.NodeID, Cursor: request.Cursor, Limit: request.Limit})
	if page.Artifacts == nil {
		page.Artifacts = []harness.TaskTreeArtifact{}
	}
	return page, err
}

// Artifact reads carry the live root-session authority. The server resolves
// the committed asset from the node; clients cannot supply a child session,
// filesystem path or an AssetRef to widen that authority.
func (a *Agent) taskArtifactRequest(ctx context.Context, raw json.RawMessage) (result any, returnErr error) {
	defer func() {
		returnErr = backgroundError(returnErr)
		var rpc *protocol.Error
		if errors.As(returnErr, &rpc) {
			if details, ok := rpc.Data.(map[string]any); ok {
				details["getMethod"] = taskTreeGetMethod
			}
		}
	}()
	reader, ok := a.service.Background.(harness.TaskArtifactReader)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Task artifact reads are unavailable")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid task artifact parameters")
	if len(raw) > 32*1024 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	fields, err := strictObject(raw, "sessionId", "rootId", "nodeId", "artifactId", "offset", "limit", "expectedSha256")
	if err != nil || fields["offset"] == nil {
		return nil, invalid
	}
	var request struct {
		SessionID      string `json:"sessionId"`
		RootID         string `json:"rootId"`
		NodeID         string `json:"nodeId"`
		ArtifactID     string `json:"artifactId"`
		Offset         int64  `json:"offset"`
		Limit          int    `json:"limit"`
		ExpectedSHA256 string `json:"expectedSha256"`
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) ||
		!validTreeID(request.RootID) || !validTreeID(request.NodeID) || !validID(request.ArtifactID, 256) ||
		request.Offset < 0 || request.Limit < 0 || request.Limit > harness.MaxTaskArtifactReadLimit {
		return nil, invalid
	}
	if request.Offset > 0 && request.ExpectedSHA256 == "" {
		return nil, invalid
	}
	if request.ExpectedSHA256 != "" {
		if len(request.ExpectedSHA256) != 64 || strings.ToLower(request.ExpectedSHA256) != request.ExpectedSHA256 {
			return nil, invalid
		}
		if _, err := hex.DecodeString(request.ExpectedSHA256); err != nil {
			return nil, invalid
		}
	}
	return reader.ReadTaskArtifact(ctx, harness.TaskActor{OwnerID: a.owner, SessionID: request.SessionID}, harness.TaskArtifactReadRequest{
		RootID: request.RootID, NodeID: request.NodeID, ArtifactID: request.ArtifactID,
		Offset: request.Offset, Limit: request.Limit, ExpectedSHA256: request.ExpectedSHA256,
	})
}
