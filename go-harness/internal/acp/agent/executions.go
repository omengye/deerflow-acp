package agent

import (
	"context"
	"encoding/json"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

const (
	getExecutionMethod       = "_deerflow/executions/get"
	resumeExecutionMethod    = "_deerflow/executions/resume"
	cancelExecutionMethod    = "_deerflow/executions/cancel"
	executionWaitingCode     = -32012
	executionConflictCode    = -32013
	executionUnresumableCode = -32014
)

func executionCapabilities() map[string]any {
	return map[string]any{"version": 1, "getMethod": getExecutionMethod, "resumeMethod": resumeExecutionMethod, "cancelMethod": cancelExecutionMethod}
}

func executionRecoveryError(code int) error {
	kind, message := "execution_waiting_input", "The session has a waiting execution. Read its state, then resume or cancel it before sending another prompt."
	if code == executionConflictCode {
		kind, message = "execution_conflict", "Execution state changed. Read the current state and use its version for the next request."
	} else if code == executionUnresumableCode {
		kind, message = "execution_unresumable", "Execution cannot resume with the current checkpoint, resources, or tool evidence. Read its state and review outstanding tool receipts."
	}
	return &protocol.Error{Code: code, Message: message, Data: map[string]any{"kind": kind, "getMethod": getExecutionMethod, "resumeMethod": resumeExecutionMethod, "cancelMethod": cancelExecutionMethod, "automaticReplay": false}}
}

type executionRequest struct {
	SessionID       string `json:"sessionId"`
	RunID           string `json:"runId"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

func decodeExecutionRequest(method string, raw json.RawMessage) (executionRequest, error) {
	var req executionRequest
	invalid := rpcError(protocol.InvalidParams, "Invalid execution request parameters")
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return req, invalid
	}
	keys := []string{"sessionId", "runId"}
	if method != getExecutionMethod {
		keys = append(keys, "expectedVersion")
	}
	fields, err := strictObject(raw, keys...)
	if err != nil || json.Unmarshal(raw, &req) != nil || !validID(req.SessionID, 256) {
		return req, invalid
	}
	_, hasRunID := fields["runId"]
	if (hasRunID || method != getExecutionMethod) && !validID(req.RunID, 256) {
		return req, invalid
	}
	if method != getExecutionMethod && req.ExpectedVersion < 1 {
		return req, invalid
	}
	return req, nil
}

func (a *Agent) executionRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	req, err := decodeExecutionRequest(method, raw)
	if err != nil {
		return nil, err
	}
	if !a.service.DurableExecutionsEnabled() {
		return nil, rpcError(protocol.MethodNotFound, "Durable execution is not available for this engine")
	}
	switch method {
	case getExecutionMethod:
		return a.service.Execution(ctx, a.owner, req.SessionID, req.RunID)
	case cancelExecutionMethod:
		return a.service.CancelExecution(ctx, a.owner, req.SessionID, harness.CancelExecutionRequest{RunID: req.RunID, ExpectedVersion: req.ExpectedVersion})
	case resumeExecutionMethod:
		return a.runResponse(func(emit harness.EventHandler) (harness.RunResult, error) {
			return a.service.ResumeExecution(hr.WithTransportCancellation(ctx), a.owner, req.SessionID, harness.ResumeExecutionRequest{RunID: req.RunID, ExpectedVersion: req.ExpectedVersion}, emit, a.permission)
		})
	default:
		return nil, rpcError(protocol.MethodNotFound, "Method not found")
	}
}

// Prompt and explicit execution resume have identical ACP output and usage
// semantics. Standard session/resume remains attachment-only.
func (a *Agent) runResponse(run func(harness.EventHandler) (harness.RunResult, error)) (any, error) {
	var mu sync.Mutex
	var usage harness.Usage
	hasUsage := false
	emit := func(ctx context.Context, e harness.RunEvent) error {
		if e.Kind == "usage" && e.Usage != nil {
			mu.Lock()
			hasUsage = true
			usage.InputTokens += e.Usage.InputTokens
			usage.OutputTokens += e.Usage.OutputTokens
			usage.TotalTokens += e.Usage.TotalTokens
			usage.Estimated = usage.Estimated || e.Usage.Estimated
			mu.Unlock()
		}
		return a.emit(ctx, e)
	}
	result, err := run(emit)
	if err != nil {
		return nil, err
	}
	response := acp.PromptResponse{StopReason: acp.StopReason(result.StopReason)}
	metadata := make(map[string]any)
	if result.Limit != "" {
		metadata["limit"] = result.Limit
	}
	if result.Execution != nil {
		metadata["execution"] = result.Execution
	}
	mu.Lock()
	if hasUsage {
		metadata["usage"] = usage
	}
	mu.Unlock()
	if len(metadata) > 0 {
		response.Meta = map[string]any{"deerflow": metadata}
	}
	return response, nil
}
