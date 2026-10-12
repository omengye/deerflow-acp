package eino

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

// Provider options are attached at the last model boundary after the shared
// budget grants an output cap. They cannot override that grant with a larger
// max_completion_tokens or thinking budget.
type roleModelTuning struct {
	provider       string
	thinking       *bool
	effort         string
	thinkingTokens int
}

func (e *Engine) roleTuning(spec agents.Spec) (*roleModelTuning, error) {
	provider := strings.ToLower(e.config.Provider)
	if spec.Model == e.config.Model && e.config.ChatModel != nil {
		provider = "injected"
	}
	if route, ok := e.config.ModelRoutes[spec.Model]; ok {
		provider = strings.ToLower(route.Provider)
	}
	cfg := harness.AgentConfig{ModelSettings: spec.ModelSettings, ThinkingEnabled: spec.ThinkingEnabled, ReasoningEffort: spec.ReasoningEffort}
	if err := cfg.ValidateModelOptions(provider, e.config.Budget.MaxOutputTokens); err != nil {
		return nil, err
	}
	if spec.ThinkingEnabled == nil && spec.ReasoningEffort == "" {
		return nil, nil
	}
	t := &roleModelTuning{provider: provider, thinking: spec.ThinkingEnabled, effort: spec.ReasoningEffort, thinkingTokens: 1024}
	if spec.ModelSettings.ThinkingBudgetTokens != nil {
		t.thinkingTokens = *spec.ModelSettings.ThinkingBudgetTokens
	}
	return t, nil
}

func (t *roleModelTuning) claudeThinking() bool {
	return t != nil && (t.provider == "claude" || t.provider == "anthropic") && t.thinking != nil && *t.thinking
}

func (t *roleModelTuning) beforeReservation(opts []model.Option) []model.Option {
	if t.claudeThinking() && model.GetCommonOptions(nil, opts...).MaxTokens == nil {
		// Account for Claude's constructor default even with disabled root quotas.
		return append(append([]model.Option(nil), opts...), model.WithMaxTokens(8192))
	}
	return opts
}

func (t *roleModelTuning) afterReservation(opts []model.Option) ([]model.Option, error) {
	if t == nil {
		return opts, nil
	}
	common := model.GetCommonOptions(nil, opts...)
	if t.claudeThinking() && (common.MaxTokens == nil || *common.MaxTokens <= t.thinkingTokens) {
		// Remaining tokens can shrink after startup validation. Never increase the
		// cap or silently disable requested thinking to fit the remaining budget.
		return nil, &budgetError{resource: "tokens"}
	}
	result := append([]model.Option(nil), opts...)
	switch t.provider {
	case "", "openai", "openai-compatible":
		if t.effort != "" {
			result = append(result, openai.WithReasoningEffort(openai.ReasoningEffortLevel(t.effort)))
		}
	case "claude", "anthropic":
		thinking := &anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
		if t.claudeThinking() {
			thinking = &anthropic.ThinkingConfigParamUnion{OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: int64(t.thinkingTokens)}}
		}
		result = append(result, claude.WithThinkingConfig(thinking))
	}
	return result, nil
}
