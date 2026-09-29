package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const (
	listBackgroundTasksMethod         = "_deerflow/tasks/list"
	getBackgroundTaskMethod           = "_deerflow/tasks/get"
	waitBackgroundTaskMethod          = "_deerflow/tasks/wait"
	cancelBackgroundTaskMethod        = "_deerflow/tasks/cancel"
	resumeBackgroundTaskMethod        = "_deerflow/tasks/resume"
	approveBackgroundTaskMethod       = "_deerflow/tasks/approve"
	listBackgroundNotificationsMethod = "_deerflow/notifications/list"
	ackBackgroundNotificationMethod   = "_deerflow/notifications/ack"
	backgroundUnavailableCode         = -32020
	backgroundConflictCode            = -32021
	backgroundUncertainCode           = -32022
)

func backgroundCapabilities(controller harness.BackgroundController) map[string]any {
	capabilities := map[string]any{
		"version": 1, "listMethod": listBackgroundTasksMethod, "getMethod": getBackgroundTaskMethod,
		"waitMethod": waitBackgroundTaskMethod, "cancelMethod": cancelBackgroundTaskMethod,
		"approveMethod": approveBackgroundTaskMethod, "maxWaitMs": 10000,
		"notifications": map[string]any{"listMethod": listBackgroundNotificationsMethod, "ackMethod": ackBackgroundNotificationMethod},
	}
	if _, ok := controller.(harness.BackgroundTaskResumer); ok {
		capabilities["resumeMethod"] = resumeBackgroundTaskMethod
	}
	if _, ok := controller.(harness.BackgroundPermissionReader); ok {
		capabilities["permissionMethod"] = getBackgroundPermissionMethod
	}
	return capabilities
}

func backgroundError(err error) error {
	if err == nil {
		return nil
	}
	code, kind, message := 0, "", ""
	switch {
	case errors.Is(err, harness.ErrBackgroundUnavailable), errors.Is(err, harness.ErrBackgroundClosed):
		code, kind, message = backgroundUnavailableCode, "background_unavailable", "Background execution is unavailable."
	case errors.Is(err, harness.ErrExecutionConflict), errors.Is(err, harness.ErrTaskOriginConflict):
		code, kind, message = backgroundConflictCode, "task_conflict", "Task state changed. Read the current task and interaction before retrying."
	case errors.Is(err, harness.ErrBackgroundUncertain), errors.Is(err, harness.ErrExecutionUnresumable):
		code, kind, message = backgroundUncertainCode, "task_reconciliation_required", "Task execution cannot safely continue. Read the task and review its tool receipts."
	case errors.Is(err, harness.ErrChildSessionBusy):
		return rpcError(protocol.ServerBusy, "Child session has an active task")
	case errors.Is(err, harness.ErrNotFound):
		return rpcError(protocol.InvalidParams, "Requested background task or notification was not found")
	case errors.Is(err, harness.ErrPermissionDenied):
		return rpcError(protocol.InvalidParams, "Background operation is not authorized")
	default:
		return err
	}
	return &protocol.Error{Code: code, Message: message, Data: map[string]any{"kind": kind, "getMethod": getBackgroundTaskMethod, "automaticReplay": false}}
}

type backgroundRequest struct {
	SessionID      string          `json:"sessionId"`
	TaskID         string          `json:"taskId"`
	NotificationID string          `json:"notificationId"`
	After          json.RawMessage `json:"after"`
	AfterVersion   int64           `json:"afterVersion"`
	Version        int64           `json:"version"`
	Limit          int             `json:"limit"`
	Approval       json.RawMessage `json:"approval"`
}

func decodeBackgroundRequest(method string, raw json.RawMessage) (backgroundRequest, error) {
	var req backgroundRequest
	invalid := rpcError(protocol.InvalidParams, "Invalid background request parameters")
	if len(raw) > 128*1024 || rejectDuplicateKeys(raw) != nil {
		return req, invalid
	}
	keys := []string{"sessionId"}
	switch method {
	case listBackgroundTasksMethod, listBackgroundNotificationsMethod:
		keys = append(keys, "after", "limit")
	case getBackgroundTaskMethod, cancelBackgroundTaskMethod:
		keys = append(keys, "taskId")
	case waitBackgroundTaskMethod:
		keys = append(keys, "taskId", "afterVersion")
	case resumeBackgroundTaskMethod:
		keys = append(keys, "taskId", "version")
	case approveBackgroundTaskMethod:
		keys = append(keys, "taskId", "approval")
	case ackBackgroundNotificationMethod:
		keys = append(keys, "notificationId")
	default:
		return req, rpcError(protocol.MethodNotFound, "Method not found")
	}
	if _, err := strictObject(raw, keys...); err != nil {
		return req, invalid
	}
	if json.Unmarshal(raw, &req) != nil || !validID(req.SessionID, 256) || req.AfterVersion < 0 || req.Limit < 0 || req.Limit > 100 {
		return req, invalid
	}
	switch method {
	case getBackgroundTaskMethod, waitBackgroundTaskMethod, cancelBackgroundTaskMethod, approveBackgroundTaskMethod, resumeBackgroundTaskMethod:
		if !validID(req.TaskID, 256) {
			return req, invalid
		}
	case ackBackgroundNotificationMethod:
		if !validID(req.NotificationID, 1024) {
			return req, invalid
		}
	}
	if method == resumeBackgroundTaskMethod && req.Version < 1 {
		return req, invalid
	}
	return req, nil
}

func decodeBackgroundApproval(raw json.RawMessage) (harness.TaskApproval, error) {
	var approval harness.TaskApproval
	invalid := rpcError(protocol.InvalidParams, "Invalid background approval parameters")
	fields, err := strictObject(raw, "id", "taskVersion", "decision", "decisions")
	if err != nil {
		return approval, invalid
	}
	if json.Unmarshal(fields["id"], &approval.ID) != nil || !validID(approval.ID, 256) || json.Unmarshal(fields["taskVersion"], &approval.TaskVersion) != nil || approval.TaskVersion < 1 {
		return approval, invalid
	}
	decision, single := fields["decision"]
	decisions, batch := fields["decisions"]
	if single == batch {
		return approval, invalid
	}
	if single {
		if json.Unmarshal(decision, &approval.Decision) != nil || !backgroundDecision(approval.Decision) {
			return approval, invalid
		}
		return approval, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(decisions, &items) != nil || len(items) == 0 || len(items) > 128 {
		return approval, invalid
	}
	type answer struct {
		IntentID string                     `json:"intentId"`
		Version  int64                      `json:"version"`
		Decision harness.PermissionDecision `json:"decision"`
	}
	answers := make([]answer, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if _, err = strictObject(item, "intentId", "version", "decision"); err != nil {
			return approval, invalid
		}
		var value answer
		if json.Unmarshal(item, &value) != nil || !validID(value.IntentID, 256) || value.Version < 1 || !backgroundDecision(value.Decision) || seen[value.IntentID] {
			return approval, invalid
		}
		seen[value.IntentID] = true
		answers = append(answers, value)
	}
	approval.Evidence, err = json.Marshal(struct {
		Decisions []answer `json:"decisions"`
	}{answers})
	if err != nil {
		return approval, invalid
	}
	return approval, nil
}

func backgroundDecision(decision harness.PermissionDecision) bool {
	return decision == harness.AllowOnce || decision == harness.RejectOnce
}

func (a *Agent) backgroundRequest(ctx context.Context, method string, raw json.RawMessage) (result any, returnErr error) {
	defer func() { returnErr = backgroundError(returnErr) }()
	controller := a.service.Background
	if controller == nil {
		return nil, rpcError(protocol.MethodNotFound, "Background execution is not available")
	}
	resumer, canResume := controller.(harness.BackgroundTaskResumer)
	if method == resumeBackgroundTaskMethod && !canResume {
		return nil, rpcError(protocol.MethodNotFound, "Background task resumption is not available")
	}
	req, err := decodeBackgroundRequest(method, raw)
	if err != nil {
		return nil, err
	}
	actor := harness.TaskActor{OwnerID: a.owner, SessionID: req.SessionID}
	switch method {
	case listBackgroundTasksMethod:
		after := ""
		if len(req.After) > 0 && (json.Unmarshal(req.After, &after) != nil || after != "" && !validID(after, 256)) {
			return nil, rpcError(protocol.InvalidParams, "Invalid background task cursor")
		}
		tasks, err := controller.BackgroundTasks(ctx, actor, after, req.Limit)
		if err != nil {
			return nil, err
		}
		if tasks == nil {
			tasks = []harness.BackgroundTask{}
		}
		return map[string]any{"tasks": tasks}, nil
	case getBackgroundTaskMethod:
		return controller.BackgroundTask(ctx, actor, req.TaskID)
	case waitBackgroundTaskMethod:
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		task, err := controller.WaitBackgroundTask(wait, actor, req.TaskID, req.AfterVersion)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return controller.BackgroundTask(ctx, actor, req.TaskID)
		}
		return task, err
	case cancelBackgroundTaskMethod:
		return controller.CancelBackgroundTask(ctx, actor, req.TaskID)
	case resumeBackgroundTaskMethod:
		return resumer.ResumeBackgroundTask(ctx, actor, req.TaskID, req.Version)
	case approveBackgroundTaskMethod:
		approval, err := decodeBackgroundApproval(req.Approval)
		if err != nil {
			return nil, err
		}
		return controller.ApproveBackgroundTask(ctx, actor, req.TaskID, approval)
	case listBackgroundNotificationsMethod:
		var after int64
		if len(req.After) > 0 && (json.Unmarshal(req.After, &after) != nil || after < 0) {
			return nil, rpcError(protocol.InvalidParams, "Invalid background notification cursor")
		}
		notifications, err := controller.BackgroundNotifications(ctx, actor, after, req.Limit)
		if err != nil {
			return nil, err
		}
		if notifications == nil {
			notifications = []harness.BackgroundNotification{}
		}
		return map[string]any{"notifications": notifications}, nil
	case ackBackgroundNotificationMethod:
		if err := controller.AcknowledgeBackgroundNotification(ctx, actor, req.NotificationID); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
	return nil, rpcError(protocol.MethodNotFound, "Method not found")
}
