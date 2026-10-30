package eino

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// The final tool set belongs to this agent invocation. DeepAgent can share
// middleware with its default child, so it must not be stored on the middleware.
type availableToolsKey struct{}

func unavailableTool(ctx context.Context, name string) bool {
	available, ok := ctx.Value(availableToolsKey{}).(map[string]bool)
	return ok && !available[name]
}

// Enable Eino's unknown-tool dispatch so its invokable middleware can record a
// rejected call and return feedback to the model instead of aborting the graph.
func unavailableToolHandler(context.Context, string, string) (string, error) {
	return "", harness.MarkToolNotExecuted(errors.New("This tool is not available to this agent; no operation was performed. Use only tools in the current tool list. If you need information or a decision from the user, ask a concise question in an ordinary assistant message and end the turn, then continue after the user's reply."))
}

func (m *toolMiddleware) rejectUnavailableTool(ctx context.Context, tc *adk.ToolContext, args string) (string, error) {
	// There is no schema or executable to authorize. Keep this rejection before
	// argument preparation, permission requests, effect admission and dispatch.
	arguments := json.RawMessage(args)
	if !json.Valid(arguments) {
		arguments = json.RawMessage(`{}`)
	}
	if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: arguments}); err != nil {
		return "", err
	}
	// Failed selections still consume the existing tool budget. The reservation
	// is settled by finish without ever being marked as dispatched.
	if err := m.admitTool(ctx, tc, string(arguments), nil); err != nil {
		return "", errors.Join(err, m.finish(ctx, tc, nil, err))
	}
	_, failure := unavailableToolHandler(ctx, tc.Name, args)
	recoverable := m.recoverableFailure(ctx, tc, failure)
	if err := m.finish(ctx, tc, nil, failure, recoverable); err != nil {
		return "", errors.Join(failure, err)
	}
	if !recoverable {
		return "", failure
	}
	return failedToolModelText("", failure), nil
}
