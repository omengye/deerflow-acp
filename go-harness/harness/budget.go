package harness

import "time"

// BudgetLimits applies to one foreground run and its delegated agents together.
// Zero values disable individual limits. Tokens without provider usage are
// estimated; MaxOutputTokens also caps each provider request's output setting.
type BudgetLimits struct {
	MaxModelCalls   int           `json:"maxModelCalls,omitempty"`
	MaxToolCalls    int           `json:"maxToolCalls,omitempty"`
	MaxTokens       int64         `json:"maxTokens,omitempty"`
	MaxOutputTokens int           `json:"maxOutputTokens,omitempty"`
	Timeout         time.Duration `json:"timeout,omitempty"`
}

// DefaultBudgetLimits is shared by the SDK and local executables. Quotas cover
// the main agent and its children together. They count logical provider calls;
// provider-internal transport retries may still incur additional charges.
func DefaultBudgetLimits() BudgetLimits {
	return BudgetLimits{MaxModelCalls: 100, MaxToolCalls: 200, MaxTokens: 200_000, MaxOutputTokens: 4096, Timeout: 30 * time.Minute}
}
