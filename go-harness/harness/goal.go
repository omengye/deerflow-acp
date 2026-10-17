package harness

import "context"

type Goal struct {
	ID        string `json:"id"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
}
type GoalEvaluation struct {
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
	Next     string   `json:"next,omitempty"`
}
type GoalAttempt struct {
	Goal          Goal   `json:"goal"`
	Continuations int    `json:"continuations"`
	LastProgress  string `json:"last_progress"`
}

// GoalStore pins a user-selected objective to a run. Evaluations alone never
// complete a goal: the containing run must also commit successfully.
type GoalStore interface {
	GoalForRun(context.Context, RunRequest) (*GoalAttempt, error)
	RecordGoalEvaluation(context.Context, RunRequest, GoalAttempt, GoalEvaluation, string, bool) error
}
