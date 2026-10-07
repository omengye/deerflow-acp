package harness

import "time"

// DefaultMaxIterations disables the optional per-agent iteration quota.
const DefaultMaxIterations = 0

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

// DefaultBudgetLimits is shared by the SDK and local executables. Model, tool
// and aggregate token quotas are opt-in; normal tasks run until completion,
// cancellation or the execution deadline. Explicit quotas cover the main agent
// and its children together and count logical calls, including failed calls.
func DefaultBudgetLimits() BudgetLimits {
	return BudgetLimits{MaxOutputTokens: 4096, Timeout: 30 * time.Minute}
}
