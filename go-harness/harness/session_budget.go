package harness

// TaskBudget selects stricter limits for new foreground tasks and their
// descendants. Zero inherits the host policy, which may itself be unlimited.
type TaskBudget struct {
	MaxTokens     int64 `json:"maxTokens"`
	MaxModelCalls int   `json:"maxModelCalls"`
	MaxToolCalls  int   `json:"maxToolCalls"`
}

func (x Session) TaskBudget() TaskBudget {
	return TaskBudget{MaxTokens: x.MaxRunTokens, MaxModelCalls: x.MaxRunModelCalls, MaxToolCalls: x.MaxRunToolCalls}
}

func (x Session) EffectiveBudget(host BudgetLimits) BudgetLimits {
	if x.MaxRunTokens > 0 && (host.MaxTokens == 0 || x.MaxRunTokens < host.MaxTokens) {
		host.MaxTokens = x.MaxRunTokens
	}
	if x.MaxRunModelCalls > 0 && (host.MaxModelCalls == 0 || x.MaxRunModelCalls < host.MaxModelCalls) {
		host.MaxModelCalls = x.MaxRunModelCalls
	}
	if x.MaxRunToolCalls > 0 && (host.MaxToolCalls == 0 || x.MaxRunToolCalls < host.MaxToolCalls) {
		host.MaxToolCalls = x.MaxRunToolCalls
	}
	return host
}

type SessionBudget struct {
	SessionID string     `json:"sessionId"`
	Version   int64      `json:"version"`
	Requested TaskBudget `json:"requested"`
	Host      TaskBudget `json:"host"`
	Effective TaskBudget `json:"effective"`
}

type SessionBudgetRequest struct {
	SessionID       string     `json:"sessionId"`
	ExpectedVersion int64      `json:"expectedVersion"`
	OperationID     string     `json:"operationId"`
	Limits          TaskBudget `json:"limits"`
}
