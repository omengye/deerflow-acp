package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	taskTreeListMethod                 = "_deerflow/tasks/tree"
	taskTreeGetMethod                  = "_deerflow/tasks/tree/get"
	taskTreeCancelMethod               = "_deerflow/tasks/tree/cancel"
	authorizeBackgroundResourcesMethod = "_deerflow/tasks/resources/authorize"
)

func taskTreeCapabilities(controller harness.BackgroundController) map[string]any {
	if _, ok := controller.(harness.TaskTreeController); !ok {
		return nil
	}
	result := map[string]any{"version": 1, "listMethod": taskTreeListMethod, "getMethod": taskTreeGetMethod}
	if _, ok := controller.(harness.TaskTreeCanceller); ok {
		result["cancelMethod"] = taskTreeCancelMethod
	}
	if _, ok := controller.(harness.TaskArtifactReader); ok {
		result["artifactReadMethod"] = taskArtifactReadMethod
	}
	if _, ok := controller.(harness.TaskArtifactLister); ok {
		result["artifactListMethod"] = taskArtifactListMethod
	}
	return result
}

func validTreeID(value string) bool {
	return validID(value, 4096) && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func (a *Agent) taskTreeRequest(ctx context.Context, method string, raw json.RawMessage) (result any, returnErr error) {
	defer func() {
		returnErr = backgroundError(returnErr)
		var rpc *protocol.Error
		if errors.As(returnErr, &rpc) {
			if details, ok := rpc.Data.(map[string]any); ok {
				details["getMethod"] = taskTreeGetMethod
			}
		}
	}()
	controller, ok := a.service.Background.(harness.TaskTreeController)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Task tree is unavailable")
	}
	canceller, canCancel := a.service.Background.(harness.TaskTreeCanceller)
	if method == taskTreeCancelMethod && !canCancel {
		return nil, rpcError(protocol.MethodNotFound, "Task tree cancellation is unavailable")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid task tree parameters")
	if len(raw) > 64*1024 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	keys := []string{"sessionId", "rootId"}
	switch method {
	case taskTreeListMethod:
		keys = append(keys, "cursor", "limit")
	case taskTreeGetMethod:
		keys = append(keys, "nodeId")
	case taskTreeCancelMethod:
		keys = append(keys, "nodeId", "expectedVersion")
	default:
		return nil, rpcError(protocol.MethodNotFound, "Method not found")
	}
	if _, err := strictObject(raw, keys...); err != nil {
		return nil, invalid
	}
	var request struct {
		SessionID       string `json:"sessionId"`
		RootID          string `json:"rootId"`
		NodeID          string `json:"nodeId"`
		Cursor          string `json:"cursor"`
		Limit           int    `json:"limit"`
		ExpectedVersion int64  `json:"expectedVersion"`
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) {
		return nil, invalid
	}
	actor := harness.TaskActor{OwnerID: a.owner, SessionID: request.SessionID}
	if method == taskTreeListMethod {
		if request.RootID != "" && !validTreeID(request.RootID) || len(request.Cursor) > 16384 || request.Limit < 0 || request.Limit > 100 {
			return nil, invalid
		}
		page, err := controller.TaskTree(ctx, actor, harness.TaskTreeQuery{RootID: request.RootID, Cursor: request.Cursor, Limit: request.Limit})
		if page.Nodes == nil {
			page.Nodes = []harness.TaskTreeNode{}
		}
		return page, err
	}
	if !validTreeID(request.RootID) || !validTreeID(request.NodeID) {
		return nil, invalid
	}
	if method == taskTreeGetMethod {
		return controller.TaskTreeNode(ctx, actor, request.RootID, request.NodeID)
	}
	if request.ExpectedVersion < 1 {
		return nil, invalid
	}
	return canceller.CancelTaskTree(ctx, actor, harness.TaskTreeCancelRequest{RootID: request.RootID, NodeID: request.NodeID, ExpectedVersion: request.ExpectedVersion})
}

func (a *Agent) authorizeBackgroundResources(ctx context.Context, raw json.RawMessage) (result any, returnErr error) {
	defer func() { returnErr = backgroundError(returnErr) }()
	controller, ok := a.service.Background.(harness.BackgroundResourceAuthorizer)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Task resource authorization is unavailable")
	}
	invalid := rpcError(protocol.InvalidParams, "Invalid task resource authorization parameters")
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return nil, invalid
	}
	if _, err := strictObject(raw, "sessionId", "taskId", "taskVersion", "resourceVersion"); err != nil {
		return nil, invalid
	}
	var request struct {
		SessionID       string `json:"sessionId"`
		TaskID          string `json:"taskId"`
		TaskVersion     int64  `json:"taskVersion"`
		ResourceVersion int64  `json:"resourceVersion"`
	}
	if json.Unmarshal(raw, &request) != nil || !validID(request.SessionID, 256) || !validID(request.TaskID, 256) || request.TaskVersion < 1 || request.ResourceVersion < 1 {
		return nil, invalid
	}
	return controller.AuthorizeBackgroundResources(ctx, harness.TaskActor{OwnerID: a.owner, SessionID: request.SessionID}, request.TaskID, request.TaskVersion, request.ResourceVersion)
}
