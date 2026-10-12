package eino

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func stoppedToolInstruction(ctx context.Context) string {
	if !interaction.HasStoppedToolRecovery(ctx) {
		return ""
	}
	return "\n\nPrevious stopped tool calls in this conversation may have partially executed; their effects remain unverified. Treat them as history, not pending work. Do not automatically resume or repeat them, and do not assume cancellation undid their effects. Follow the latest user request. Before repeating an affected operation, inspect its actual state and ask for confirmation when needed."
}

type stoppedToolHistory struct {
	adk.BaseChatModelAgentMiddleware
}

func (*stoppedToolHistory) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if !interaction.HasStoppedToolRecovery(ctx) {
		return ctx, state, nil
	}
	boundary := -1
	for i := len(state.Messages) - 1; i >= 0; i-- {
		message := state.Messages[i]
		if message != nil && message.Role == schema.User && !isOutputContinuation(message) {
			boundary = i
			break
		}
	}
	if boundary < 1 {
		return ctx, state, nil
	}
	var repaired []*schema.Message
	for i := 0; i < len(state.Messages); i++ {
		message := state.Messages[i]
		if repaired != nil {
			repaired = append(repaired, message)
		}
		if i >= boundary || message == nil || message.Role != schema.Assistant || len(message.ToolCalls) == 0 {
			continue
		}
		end := i + 1
		observed := make(map[string]bool)
		for end < boundary && state.Messages[end] != nil && state.Messages[end].Role == schema.Tool {
			observed[state.Messages[end].ToolCallID] = true
			end++
		}
		var missing []*schema.Message
		for _, call := range message.ToolCalls {
			if !observed[call.ID] {
				missing = append(missing, schema.ToolMessage("Interrupted: no final tool result was recorded before the previous turn stopped. Execution and partial effects are unconfirmed. Do not automatically retry; check the actual state first.", call.ID, schema.WithToolName(call.Function.Name)))
			}
		}
		if len(missing) > 0 && repaired == nil {
			repaired = append([]*schema.Message(nil), state.Messages[:i+1]...)
		}
		if repaired != nil {
			repaired = append(repaired, state.Messages[i+1:end]...)
			repaired = append(repaired, missing...)
		}
		i = end - 1
	}
	if repaired == nil {
		return ctx, state, nil
	}
	// Repair only historical groups before the fresh user input. Never close
	// this turn's pending tools or modify persisted business receipt evidence.
	copy := *state
	copy.Messages = repaired
	return ctx, &copy, nil
}
