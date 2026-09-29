package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

const processBackgroundNotificationMethod = "_deerflow/notifications/process"

func (a *Agent) notificationProcessor() (harness.BackgroundNotificationProcessor, error) {
	processor, ok := a.service.Background.(harness.BackgroundNotificationProcessor)
	if !ok {
		return nil, rpcError(protocol.MethodNotFound, "Background notification processing is not available")
	}
	return processor, nil
}

func (a *Agent) processBackgroundNotification(ctx context.Context, raw json.RawMessage) (any, error) {
	processor, err := a.notificationProcessor()
	if err != nil {
		return nil, err
	}
	req, err := decodeBackgroundRequest(processBackgroundNotificationMethod, raw)
	if err != nil {
		return nil, err
	}
	result, err := a.runResponse(func(emit harness.EventHandler) (harness.RunResult, error) {
		return processor.ProcessBackgroundNotification(hr.WithTransportCancellation(ctx), harness.TaskActor{OwnerID: a.owner, SessionID: req.SessionID}, req.NotificationID, emit, a.permission)
	})
	var limit *budget.LimitError
	// Continuations recover through the foreground execution methods. Do not
	// map their conflicts to background task approval/resumption endpoints.
	switch {
	case errors.As(err, &limit):
		return result, &protocol.Error{Code: -32015, Message: "The originating shared budget cannot admit a continuation.", Data: map[string]any{"kind": "origin_budget_limit", "resource": limit.Resource, "temporary": limit.Temporary, "automaticReplay": false}}
	case errors.Is(err, harness.ErrBackgroundUnavailable), errors.Is(err, harness.ErrBackgroundClosed):
		return result, backgroundError(err)
	case errors.Is(err, harness.ErrTaskOriginConflict):
		return result, errors.Join(harness.ErrExecutionConflict, err)
	case errors.Is(err, harness.ErrBackgroundUncertain):
		return result, errors.Join(harness.ErrExecutionUnresumable, err)
	case errors.Is(err, harness.ErrPermissionDenied):
		return result, rpcError(protocol.InvalidParams, "Background notification processing is not authorized")
	default:
		return result, err
	}
}
