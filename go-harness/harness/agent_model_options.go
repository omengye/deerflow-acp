package harness

import (
	"fmt"
	"slices"
	"strings"
)

// ValidateModelOptions checks configured roles against every selectable parent
// model, including inherited models. It is shared by config validation and the
// execution adapter; validation never constructs a provider or uses credentials.
func (c SubagentsConfig) ValidateModelOptions(providers map[string]string, maxOutputTokens int) error {
	models := make([]string, 0, len(providers))
	for name := range providers {
		models = append(models, name)
	}
	slices.Sort(models)
	for _, collection := range []struct {
		name  string
		roles map[string]AgentConfig
	}{{"agents", c.Agents}, {"custom_agents", c.CustomAgents}} {
		names := make([]string, 0, len(collection.roles))
		for name := range collection.roles {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			role := collection.roles[name]
			selected := models
			if role.Model != "" && role.Model != "inherit" {
				selected = []string{role.Model}
			}
			for _, model := range selected {
				provider, ok := providers[model]
				if !ok {
					return fmt.Errorf("subagents.%s.%s.model: unknown model %q", collection.name, name, model)
				}
				if err := role.ValidateModelOptions(provider, maxOutputTokens); err != nil {
					return fmt.Errorf("subagents.%s.%s (model %q): %w", collection.name, name, model, err)
				}
			}
		}
	}
	return nil
}

// ValidateModelOptions describes the public options supported by the pinned
// Eino adapters. Individual remote models can still reject an otherwise valid
// provider option; this function does not infer capabilities from model names.
func (c AgentConfig) ValidateModelOptions(provider string, maxOutputTokens int) error {
	provider = strings.ToLower(provider)
	thinking := c.ThinkingEnabled != nil && *c.ThinkingEnabled
	if c.ThinkingEnabled != nil && !slices.Contains([]string{"claude", "anthropic"}, provider) {
		return fmt.Errorf("thinking_enabled is not supported by provider %q", provider)
	}
	if c.ReasoningEffort != "" {
		if !slices.Contains([]string{"", "openai", "openai-compatible"}, provider) {
			return fmt.Errorf("reasoning_effort is not supported by provider %q", provider)
		}
		// The endpoint owns the final model capability check; this is only the
		// shared OpenAI-compatible wire contract allowlist.
		if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, c.ReasoningEffort) {
			return fmt.Errorf("reasoning_effort %q is not supported by provider %q", c.ReasoningEffort, provider)
		}
		if c.ThinkingEnabled != nil && !thinking {
			return fmt.Errorf("reasoning_effort requires thinking_enabled not to be false")
		}
	}
	if c.ModelSettings.ThinkingBudgetTokens != nil && (!thinking || provider != "claude" && provider != "anthropic") {
		return fmt.Errorf("model_settings.thinking_budget_tokens requires Claude thinking_enabled=true")
	}
	if provider == "claude" || provider == "anthropic" {
		if p := c.ModelSettings.Temperature; p != nil && (*p > 1 || thinking && *p != 1) {
			return fmt.Errorf("model_settings.temperature must be 0..1 for Claude, and 1 when thinking is enabled")
		}
		if thinking {
			budget := 1024
			if c.ModelSettings.ThinkingBudgetTokens != nil {
				budget = *c.ModelSettings.ThinkingBudgetTokens
			}
			if budget < 1024 {
				return fmt.Errorf("model_settings.thinking_budget_tokens must be at least 1024")
			}
			cap := maxOutputTokens
			if p := c.ModelSettings.MaxTokens; p != nil && (cap == 0 || *p < cap) {
				cap = *p
			}
			// Claude's constructor default is 8192 when the host is unbounded.
			if cap == 0 {
				cap = 8192
			}
			if cap <= budget {
				return fmt.Errorf("model_settings.thinking_budget_tokens (%d) must be less than the effective output token limit (%d)", budget, cap)
			}
		}
	}
	return nil
}
