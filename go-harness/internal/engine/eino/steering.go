package eino

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

const steeringCursorKey = "deerflow_steering_cursor_v1"
const steeringSequenceKey = "deerflow_steering_sequence_v1"

func (e *Engine) SteeringSupported() bool { return e.DurableExecutions() }

type steeringHistory struct {
	adk.BaseChatModelAgentMiddleware
}

func (*steeringHistory) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	feed, ok := interaction.Steering(ctx)
	if !ok || feed.Read == nil || IsDelegatedToolContext(ctx) {
		return ctx, state, nil
	}
	if _, active, err := CurrentRole(ctx); err != nil {
		return ctx, state, err
	} else if active {
		return ctx, state, nil
	}
	value, exists, err := adk.GetRunLocalValue(ctx, steeringCursorKey)
	if err != nil {
		return ctx, state, err
	}
	var cursor int64
	if exists {
		var valid bool
		cursor, valid = value.(int64)
		if !valid || cursor < 0 {
			return ctx, state, harness.ErrExecutionConflict
		}
	}
	items, err := feed.Read(ctx, cursor)
	if err != nil {
		return ctx, state, err
	}
	if len(items) == 0 {
		return ctx, state, nil
	}
	copy := *state
	copy.Messages = append([]*schema.Message(nil), state.Messages...)
	for _, item := range items {
		if item.Sequence <= cursor || (item.Status != "accepted" && item.Status != "applied") {
			return ctx, state, harness.ErrExecutionConflict
		}
		message := schema.UserMessage(item.Text)
		message.Extra = map[string]any{steeringSequenceKey: strconv.FormatInt(item.Sequence, 10)}
		copy.Messages = append(copy.Messages, message)
		cursor = item.Sequence
	}
	// Messages and cursor are serialized in the same native agent checkpoint.
	// On recovery an older cursor rehydrates any instruction missing from that
	// checkpoint; on a normal retry the instruction is never appended twice.
	if err = adk.SetRunLocalValue(ctx, steeringCursorKey, cursor); err != nil {
		return ctx, state, err
	}
	return ctx, &copy, nil
}

func (m *trackedModel) applySteering(ctx context.Context, input []*schema.Message) error {
	if !m.nativeEndpoint || m.role || IsDelegatedToolContext(ctx) {
		return nil
	}
	feed, ok := interaction.Steering(ctx)
	if !ok || feed.Applied == nil {
		return nil
	}
	var sequences []int64
	for _, message := range input {
		if message == nil || message.Role != schema.User {
			continue
		}
		if raw, ok := message.Extra[steeringSequenceKey]; ok {
			text, valid := raw.(string)
			sequence, err := strconv.ParseInt(text, 10, 64)
			if !valid || err != nil || sequence < 1 {
				return fmt.Errorf("%w: invalid steering identity", harness.ErrExecutionConflict)
			}
			sequences = append(sequences, sequence)
		}
	}
	if len(sequences) == 0 {
		return nil
	}
	return feed.Applied(ctx, sequences)
}
