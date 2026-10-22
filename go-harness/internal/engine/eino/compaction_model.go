package eino

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type compactionModel struct {
	inner model.BaseModel[*schema.Message]
}

func (m *compactionModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		summary, err := m.inner.Generate(ctx, input, opts...)
		if err != nil {
			return nil, err // Never replay provider failures or bypass budget admission.
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		failure := validateCompactionSummary(summary, attempt)
		if failure == nil {
			return summary, nil
		}
		if attempt == 2 || failure.Reason == harness.CompactionInvalid {
			return nil, failure
		}
		// Regenerate from intact input, never from the rejected partial summary.
		// The inner tracked model reserves and settles each call independently.
		retryInput := append([]*schema.Message(nil), input...)
		retryInput = append(retryInput, schema.UserMessage(compactionRetryInstruction))
		input = retryInput
	}
}

func (m *compactionModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	summary, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{summary}), nil
}

func validateCompactionSummary(summary *schema.Message, attempts int) *harness.CompactionError {
	failure := &harness.CompactionError{Reason: harness.CompactionInvalid, Attempts: attempts}
	if summary == nil {
		return failure
	}
	failure.SummaryBytes = len(summary.Content)
	if summary.ResponseMeta != nil {
		switch summary.ResponseMeta.FinishReason {
		case "stop", "end_turn", "length", "max_tokens", "tool_calls", "content_filter":
			failure.FinishReason = summary.ResponseMeta.FinishReason
		}
	}
	switch {
	case summary.Role != schema.Assistant || len(summary.ToolCalls) != 0 || len(summary.AssistantGenMultiContent) != 0 || !utf8.ValidString(summary.Content):
	case outputExhausted(summary):
		failure.Reason = harness.CompactionTruncated
	case strings.TrimSpace(summary.Content) == "":
		failure.Reason = harness.CompactionEmpty
	case len(summary.Content) > maxSummaryBytes:
		failure.Reason = harness.CompactionOversized
	default:
		return nil
	}
	return failure
}

// Hidden summaries have their own non-thinking policy. Only send switches
// declared by the host; normal answers and role recipes remain unchanged.
func (e *Engine) compactionTuning(selectedModel string) *roleModelTuning {
	provider := e.config.Provider
	if route, ok := e.config.ModelRoutes[selectedModel]; ok {
		provider = route.Provider
	}
	tuning := &roleModelTuning{provider: strings.ToLower(provider)}
	switch e.config.ModelThinking[selectedModel].Parameter {
	case "enable_thinking":
		tuning.extraFields = map[string]any{"enable_thinking": false}
	case "thinking_type":
		tuning.extraFields = map[string]any{"thinking": map[string]any{"type": "disabled"}}
	case "reasoning_effort":
		tuning.effort = "none"
	case "claude":
		disabled := false
		tuning.thinking = &disabled
	default:
		return nil
	}
	return tuning
}
