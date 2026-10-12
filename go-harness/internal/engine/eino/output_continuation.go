package eino

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const maxOutputContinuations = 4
const outputContinuationKey = "deerflow_output_continuation"
const outputConsolidatedKey = "deerflow_output_consolidated"

// Native AgentTool/background executors return only the last output event.
// Consolidate a continued final answer for them without replaying its text in
// the harness UI. Run-local state keeps concurrent roles and resumes isolated.
type outputContinuationHistory struct {
	adk.BaseChatModelAgentMiddleware
}

func (*outputContinuationHistory) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	return ctx, state, adk.SetRunLocalValue(ctx, outputContinuationKey, "")
}

func (*outputContinuationHistory) AfterModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	value, _, err := adk.GetRunLocalValue(ctx, outputContinuationKey)
	if err != nil {
		return ctx, state, err
	}
	prefix, _ := value.(string)
	if prefix == "" || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	last := state.Messages[len(state.Messages)-1]
	if last == nil || last.Role != schema.Assistant || len(last.ToolCalls) != 0 {
		return ctx, state, nil
	}
	merged := *last
	merged.Content = prefix + continuationText(last)
	merged.AssistantGenMultiContent = nil
	for _, part := range last.AssistantGenMultiContent {
		if part.Type != schema.ChatMessagePartTypeText {
			merged.AssistantGenMultiContent = append(merged.AssistantGenMultiContent, part)
		}
	}
	merged.Extra = maps.Clone(last.Extra)
	if merged.Extra == nil {
		merged.Extra = map[string]any{}
	}
	merged.Extra[outputConsolidatedKey] = true
	start := len(state.Messages) - 1
	for start > 0 && state.Messages[start-1] != nil && state.Messages[start-1].Extra[outputContinuationKey] == true {
		start--
	}
	state.Messages = append(append([]*schema.Message(nil), state.Messages[:start]...), &merged)
	event := outputContextReplacement(state.Messages, "output_consolidated")
	event.Output = &adk.AgentOutput{MessageOutput: &adk.MessageVariant{Role: schema.Assistant, Message: &merged}}
	return ctx, state, adk.SendEvent(ctx, event)
}

func outputContextReplacement(input []*schema.Message, reason string) *adk.AgentEvent {
	for len(input) > 0 && input[0] != nil && input[0].Role == schema.System {
		input = input[1:]
	}
	return &adk.AgentEvent{SessionEventVariant: &adk.SessionEventVariant[*schema.Message]{Event: &adk.SessionEvent[*schema.Message]{Kind: adk.SessionEventMessagesReplaced, MessagesReplaced: &input, Extra: map[string]any{"_eino_reason": reason}}}}
}

func outputConsolidated(msg *schema.Message) bool {
	return msg != nil && msg.Extra[outputConsolidatedKey] == true
}

type outputContinuationError struct{ reason string }

func (e *outputContinuationError) Error() string {
	return "The model reached its per-response output limit and automatic continuation could not finish (" + e.reason + "). Partial output was preserved; no truncated tool calls were executed. Increase the per-response output allowance or reduce reasoning effort before continuing."
}

func outputExhausted(msg *schema.Message) bool {
	return msg != nil && msg.ResponseMeta != nil && (msg.ResponseMeta.FinishReason == "length" || msg.ResponseMeta.FinishReason == "max_tokens")
}

func isOutputContinuation(msg *schema.Message) bool {
	return msg != nil && msg.Role == schema.User && msg.Extra[outputContinuationKey] == true
}

// Use the native model retry boundary, not a new agent run: completed tools are
// never replayed, and each provider request still passes through trackedModel.
func outputContinuationPolicy(compaction adk.ChatModelAgentMiddleware) *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries:  maxOutputContinuations,
		BackoffFunc: func(context.Context, int) time.Duration { return 0 },
		ShouldRetry: func(ctx context.Context, attempt *adk.RetryContext) *adk.RetryDecision {
			if attempt.Err != nil || !outputExhausted(attempt.OutputMessage) {
				return nil // Never guess from punctuation, or replay failed provider I/O.
			}
			if err := ctx.Err(); err != nil {
				return &adk.RetryDecision{RewriteError: err}
			}
			stop := func(reason string) *adk.RetryDecision {
				// Failed streams remain in the audit log, but are not reconstructed
				// as model context. Preserve their safe text explicitly for follow-up.
				input := append([]*schema.Message(nil), attempt.InputMessages...)
				if text := continuationText(attempt.OutputMessage); text != "" {
					input = append(input, schema.AssistantMessage(text, nil))
				}
				if err := adk.SendEvent(ctx, outputContextReplacement(input, "output_incomplete")); err != nil {
					return &adk.RetryDecision{RewriteError: err}
				}
				return &adk.RetryDecision{RewriteError: &outputContinuationError{reason: reason}}
			}
			if attempt.RetryAttempt > maxOutputContinuations {
				return stop("continuation limit reached")
			}
			msg := attempt.OutputMessage
			text := continuationText(msg)
			for _, part := range msg.AssistantGenMultiContent {
				if part.Type != schema.ChatMessagePartTypeText {
					return stop("non-text output cannot be safely continued")
				}
			}
			noProgress := strings.TrimSpace(text) == ""
			for i := len(attempt.InputMessages) - 1; attempt.RetryAttempt > 1 && i >= 0; i-- {
				previous := attempt.InputMessages[i]
				if isOutputContinuation(previous) {
					if noProgress && previous.Extra["no_progress"] == true {
						return stop("repeated output exhaustion without visible progress")
					}
					if i > 0 && text != "" && attempt.InputMessages[i-1] != nil && strings.TrimSpace(attempt.InputMessages[i-1].Content) == strings.TrimSpace(text) {
						return stop("continuation repeated the same partial answer")
					}
					break
				}
			}
			input := append([]*schema.Message(nil), attempt.InputMessages...)
			// A truncated tool group is rejected in its entirety, even when an
			// individual argument happens to be valid JSON. Do not turn private
			// reasoning or incomplete provider metadata into assistant text.
			if text != "" {
				input = append(input, &schema.Message{Role: schema.Assistant, Content: text, Extra: map[string]any{outputContinuationKey: true}})
				value, _, err := adk.GetRunLocalValue(ctx, outputContinuationKey)
				if err != nil {
					return &adk.RetryDecision{RewriteError: err}
				}
				prefix, _ := value.(string)
				if err := adk.SetRunLocalValue(ctx, outputContinuationKey, prefix+text); err != nil {
					return &adk.RetryDecision{RewriteError: err}
				}
			}
			// Native retries do not rerun BeforeModelRewriteState. Run only the
			// existing guarded compactor, before adding the synthetic user note so
			// the actual request and latest partial answer remain the active turn.
			if compaction != nil {
				common := model.GetCommonOptions(nil, attempt.Options...)
				_, state, err := compaction.BeforeModelRewriteState(ctx, &adk.ChatModelAgentState{Messages: input, ToolInfos: common.Tools, DeferredToolInfos: common.DeferredTools}, &adk.ModelContext{Tools: common.Tools})
				if err != nil {
					return &adk.RetryDecision{RewriteError: err}
				}
				input = state.Messages
			}
			note := "[Host output-limit recovery] Your previous response exhausted its per-response output allowance. Continue the same task from the end of the visible answer without repeating it or restarting completed work. Keep the continuation concise and use less reasoning."
			if len(msg.ToolCalls) > 0 {
				note += " The entire tool-call group in that response was discarded and NONE of those calls executed. Reissue any still-needed calls with complete arguments, using smaller operations. Do not repeat earlier completed tool calls."
			}
			input = append(input, &schema.Message{Role: schema.User, Content: note, Extra: map[string]any{outputContinuationKey: true, "no_progress": noProgress}})
			// The native retry updates graph state, but does not persist a history
			// mutation itself. Persist the same safe context for the next turn or
			// restart, excluding runtime-generated leading system instructions.
			if err := adk.SendEvent(ctx, outputContextReplacement(input, "output_continued")); err != nil {
				return &adk.RetryDecision{RewriteError: err}
			}
			return &adk.RetryDecision{Retry: true, ModifiedInputMessages: input, PersistModifiedInputMessages: true, RejectReason: "Continuing after the per-response output limit; truncated tool calls were not executed."}
		},
	}
}

func continuationText(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Content != "" {
		return msg.Content
	}
	var text strings.Builder
	for _, part := range msg.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func modelWillContinue(err error) bool {
	var retry *adk.WillRetryError
	return errors.As(err, &retry)
}
