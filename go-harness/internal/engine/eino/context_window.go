package eino

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func classifyModelContextError(err error) error {
	var api *openaiapi.APIError
	if errors.Is(err, harness.ErrContextLimit) || !errors.As(err, &api) || api.HTTPStatusCode != 400 {
		return err
	}
	code, _ := api.Code.(string)
	message := strings.ToLower(strings.TrimSpace(api.Message))
	if code == "context_length_exceeded" || strings.HasPrefix(message, "the input token count exceeds the maximum number of tokens allowed") || strings.HasPrefix(message, "this model's maximum context length is") {
		return fmt.Errorf("%w: %w", harness.ErrContextLimit, err)
	}
	return err
}

// Estimates remain estimates; reported provider usage is authoritative at settlement.
// Count CJK runes separately so Chinese text is not treated as four-byte English.
func estimatedTextTokens(text []byte) int64 {
	var ascii, other int64
	for _, r := range string(text) {
		if r < 128 {
			ascii++
		} else if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			other += 2
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func estimateModelTokens(messages []*schema.Message, tools []*schema.ToolInfo) (int64, error) {
	encoded, images, err := budgetMessageEncoding(messages)
	if err != nil {
		return 0, err
	}
	schemas, err := json.Marshal(tools)
	if err != nil {
		return 0, err
	}
	return estimatedTextTokens(encoded) + estimatedTextTokens(schemas) + int64(len(messages))*8 + images, nil
}

func outputReserve(limit int, settings harness.AgentModelSettings) int {
	if settings.MaxTokens != nil && *settings.MaxTokens > 0 && (limit == 0 || *settings.MaxTokens < limit) {
		limit = *settings.MaxTokens
	}
	if limit == 0 {
		limit = 4096
	}
	return limit
}

func windowCompaction(c harness.CompactionConfig, window, output int) harness.CompactionConfig {
	threshold := 32_000 // Only a fallback when the host has no model window.
	if window > 0 {
		// Leave the full output cap and the same estimate margin as preflight,
		// then start early enough to also fit a summarization instruction.
		available := max(1, window-output-max(16, window/50))
		threshold = max(1, available*3/4)
	}
	if c.ContextTokens == 0 || window > 0 && c.ContextTokens > threshold {
		c.ContextTokens = threshold
	}
	return c
}

func (m *trackedModel) windowOptions(messages []*schema.Message, opts []model.Option) ([]model.Option, error) {
	if m.contextWindow == 0 {
		return opts, nil
	}
	common := model.GetCommonOptions(&model.Options{}, opts...)
	tokens, err := estimateModelTokens(messages, common.Tools)
	if err != nil {
		return nil, err
	}
	remaining := int64(m.contextWindow) - tokens - int64(max(16, m.contextWindow/50))
	if remaining < 1 {
		return nil, fmt.Errorf("%w: estimated input %d exceeds configured window %d; compact history or use a larger-window model", harness.ErrContextLimit, tokens, m.contextWindow)
	}
	output := outputReserve(m.budget.limits.MaxOutputTokens, m.settings)
	if common.MaxTokens != nil && *common.MaxTokens > 0 {
		output = min(output, *common.MaxTokens)
	}
	return append(append([]model.Option(nil), opts...), model.WithMaxTokens(min(output, int(remaining)))), nil
}
