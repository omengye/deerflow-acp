package harness

import "time"

// DefaultMaxIterations disables the optional per-agent iteration quota.
const DefaultMaxIterations = 0

// Explicit execution limits may span days. Zero remains the unlimited default.
const MaxExecutionTimeoutSeconds = 10 * 365 * 24 * 60 * 60

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
// and aggregate token quotas are opt-in; normal tasks run until completion or
// cancellation. A host can opt into an execution deadline. Explicit quotas cover
// the main agent and its children together, including failed logical calls.
func DefaultBudgetLimits() BudgetLimits {
	return BudgetLimits{MaxOutputTokens: 4096}
}

// Long-task safety checkpoint used by the local ACP executables when model or
// tool call quotas are configured as 0. The values are sized so an ordinary
// three-hour task, including parallel subagents sharing the root budget, does
// not reach them: about one model call every 2.7s for three hours, with up to
// three tool calls per model call. A runaway loop still stops after a bounded
// amount of work. Reaching the checkpoint ends the run like any quota stop
// (max_turn_requests); the conversation is kept and the user continues by
// sending another message, which starts a fresh allowance.
const (
	LongTaskSafetyModelCalls = 4000
	LongTaskSafetyToolCalls  = 12000
)

// WithLongTaskSafety replaces disabled (zero) call quotas with the long-task
// safety checkpoint. Explicit positive limits are kept unchanged.
func (l BudgetLimits) WithLongTaskSafety() BudgetLimits {
	if l.MaxModelCalls == 0 {
		l.MaxModelCalls = LongTaskSafetyModelCalls
	}
	if l.MaxToolCalls == 0 {
		l.MaxToolCalls = LongTaskSafetyToolCalls
	}
	return l
}
