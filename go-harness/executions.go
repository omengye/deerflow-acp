package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Execution reads a logical run. An empty runID selects the latest execution
// in the attached session, allowing discovery after a disconnected prompt.
func (c *Client) Execution(ctx context.Context, sessionID, runID string) (harness.ExecutionState, error) {
	done, err := c.operation()
	if err != nil {
		return harness.ExecutionState{}, err
	}
	defer done()
	return c.service.Execution(ctx, c.owner, sessionID, runID)
}

// ResumeExecution resumes the same accepted input after asking for fresh
// permission. PermissionCancelled leaves the execution waiting for later input.
// It never accepts a replacement prompt or native checkpoint address.
func (c *Client) ResumeExecution(ctx context.Context, sessionID string, request harness.ResumeExecutionRequest, emit harness.EventHandler, approve harness.PermissionHandler) (harness.RunResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.RunResult{}, err
	}
	defer done()
	return c.service.ResumeExecution(ctx, c.owner, sessionID, request, emit, approve)
}

// CancelExecution closes an idle waiting execution. Cancel stops a currently
// running call; both operations retain durable evidence of tool effects.
func (c *Client) CancelExecution(ctx context.Context, sessionID string, request harness.CancelExecutionRequest) (harness.ExecutionState, error) {
	done, err := c.operation()
	if err != nil {
		return harness.ExecutionState{}, err
	}
	defer done()
	return c.service.CancelExecution(ctx, c.owner, sessionID, request)
}
