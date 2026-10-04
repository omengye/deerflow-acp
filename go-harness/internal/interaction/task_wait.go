package interaction

import (
	"context"
	"github.com/cloudwego/eino/schema"
)

// The runtime commits staged delivery only with a saved checkpoint or a
// completed attempt. Merely fetching a result is not model delivery.
type TaskResultDelivery struct {
	Wait        TaskWait
	TaskVersion int64
}
type TaskResultObserver interface {
	ObserveTaskResult(context.Context, TaskResultDelivery) error
}

// TaskWait is created by the host's task_wait tool, never from resume input.
// It carries no grant to execute an effect or approve a descendant.
type TaskWait struct {
	TaskID                                 string
	AfterVersion                           int64
	CallID, SessionID, RunID, RootBudgetID string
	RoleID                                 string `json:",omitempty"`
}

type TaskWaitInterrupt struct{ Wait TaskWait }
type TaskWaitBinding struct {
	Wait              TaskWait
	NativeInterruptID string
}

func init() {
	schema.Register[TaskWait]()
	schema.Register[*TaskWaitInterrupt]()
}
