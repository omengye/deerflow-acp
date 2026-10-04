package agent

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type sessionUpdatePump struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Automatic output belongs to the live connection, not a completed prompt's
// request context. There is at most one pump per attached session.
func (a *Agent) startSessionUpdates(sessionID string) {
	if !a.service.DurableExecutionsEnabled() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.updatesCtx == nil || a.updatesCtx.Err() != nil || a.updates[sessionID] != nil {
		return
	}
	if a.updates == nil {
		a.updates = make(map[string]*sessionUpdatePump)
	}
	ctx, cancel := context.WithCancel(a.updatesCtx)
	pump := &sessionUpdatePump{cancel: cancel, done: make(chan struct{})}
	a.updates[sessionID] = pump
	a.updatesWG.Add(1)
	go func() {
		defer a.updatesWG.Done()
		defer cancel()
		defer func() {
			a.mu.Lock()
			delete(a.updates, sessionID)
			close(pump.done)
			a.mu.Unlock()
		}()
		var reported bool
		var operationID, runID string
		err := a.service.ServeSessionUpdates(ctx, a.owner, sessionID, harness.SessionUpdates{
			Started: func(ctx context.Context, state harness.ExecutionState) error {
				operationID, runID = rand.Text(), state.RunID
				return a.connectionError(ctx, a.peer.Notify(ctx, updatedExecutionMethod, map[string]any{
					"sessionId": sessionID, "phase": "started", "operationId": operationID,
					"runId": runID, "execution": state,
				}))
			},
			Events: a.emit, Permission: a.permission,
			Result: func(ctx context.Context, result harness.RunResult, runErr error) error {
				reported = runErr != nil
				err := a.executionUpdatedOperation(ctx, sessionID, operationID, runID, result, runErr)
				operationID, runID = "", ""
				return err
			},
		})
		if err != nil && !reported && ctx.Err() == nil && !errors.Is(err, harness.ErrNotAttached) {
			_ = a.executionUpdated(ctx, sessionID, harness.RunResult{}, err)
		}
	}()
}

func (a *Agent) stopSessionUpdates(sessionID string) {
	a.mu.Lock()
	pump := a.updates[sessionID]
	if pump != nil {
		pump.cancel()
	}
	a.mu.Unlock()
	if pump != nil {
		<-pump.done
	}
}

func (a *Agent) executionUpdated(ctx context.Context, sessionID string, result harness.RunResult, err error) error {
	return a.executionUpdatedOperation(ctx, sessionID, "", "", result, err)
}

func (a *Agent) executionUpdatedOperation(ctx context.Context, sessionID, operationID, runID string, result harness.RunResult, err error) error {
	payload := map[string]any{"stopReason": result.StopReason}
	if result.Execution != nil {
		payload["execution"] = result.Execution
	}
	if result.Background != nil {
		payload["backgroundTask"] = result.Background
	}
	if result.Limit != "" {
		payload["limit"] = result.Limit
	}
	params := map[string]any{"sessionId": sessionID, "phase": "finished", "result": payload}
	if operationID != "" {
		params["operationId"], params["runId"] = operationID, runID
	}
	if err != nil {
		params["error"] = executionUpdateError(err)
	}
	return a.connectionError(ctx, a.peer.Notify(ctx, updatedExecutionMethod, params))
}

// Notifications must use the same public errors as JSON-RPC responses. Raw
// provider/storage errors are diagnostic data and may contain credentials.
func executionUpdateError(err error) *protocol.Error {
	var rpc *protocol.Error
	if errors.As(mapError(err), &rpc) {
		return rpc
	}
	return &protocol.Error{Code: protocol.InternalError, Message: "Internal error"}
}
