package harness

import (
	"fmt"
	"slices"
	"strings"
)

// ThinkingConfig is a host-declared wire contract, never inferred from a model
// name. Empty means unconfigured; unsupported explicitly declares no switch.
type ThinkingConfig struct {
	Parameter    string `json:"parameter,omitempty"`
	BudgetTokens int    `json:"budgetTokens,omitempty"`
}

func (c ThinkingConfig) EnabledBudget() int {
	if c.BudgetTokens == 0 {
		return 1024
	}
	return c.BudgetTokens
}

func (c ThinkingConfig) SupportsSwitch() bool {
	return c.Parameter != "" && c.Parameter != "unsupported"
}

func (c ThinkingConfig) Availability() string {
	if c.Parameter == "" {
		return "unconfigured"
	}
	if !c.SupportsSwitch() {
		return "unsupported"
	}
	return "configured"
}

// A shared effort parameter cannot represent explicit on with an unknown
// provider default: that default may itself disable reasoning.
func (c ThinkingConfig) ValidateSelection(mode, effort string) error {
	if !ThinkingModeAllowed(c, mode) {
		return fmt.Errorf("thinking mode is not configured or supported for this model")
	}
	if c.Parameter == "reasoning_effort" && mode == "on" && (effort == "" || effort == "default") {
		return fmt.Errorf("select an explicit reasoning effort before enabling thinking; the model default is unknown")
	}
	return nil
}

func (c ThinkingConfig) Validate(provider string, efforts []string, maxOutputTokens int) error {
	provider = strings.ToLower(provider)
	switch c.Parameter {
	case "", "unsupported":
	case "enable_thinking", "thinking_type", "reasoning_effort":
		if !slices.Contains([]string{"", "openai", "openai-compatible"}, provider) {
			return fmt.Errorf("thinking_parameter %q requires an OpenAI-compatible provider", c.Parameter)
		}
		if c.Parameter == "reasoning_effort" && len(efforts) == 0 {
			return fmt.Errorf("thinking_parameter reasoning_effort requires at least one reasoning_efforts value for enabling thinking")
		}
	case "claude":
		if provider != "claude" && provider != "anthropic" {
			return fmt.Errorf("thinking_parameter claude requires a Claude provider")
		}
		enabled, budget := true, c.EnabledBudget()
		return (AgentConfig{ThinkingEnabled: &enabled, ModelSettings: AgentModelSettings{ThinkingBudgetTokens: &budget}}).ValidateModelOptions(provider, maxOutputTokens)
	default:
		return fmt.Errorf("unsupported thinking_parameter %q", c.Parameter)
	}
	if c.BudgetTokens != 0 {
		return fmt.Errorf("thinking_budget_tokens requires thinking_parameter claude")
	}
	return nil
}

func ThinkingModeAllowed(config ThinkingConfig, mode string) bool {
	return mode == "" || mode == "default" || config.SupportsSwitch() && (mode == "on" || mode == "off")
}
