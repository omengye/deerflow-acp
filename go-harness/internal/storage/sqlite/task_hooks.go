package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

// TaskHooks extend the provider's transaction without replacing its lifecycle
// state machine. Hooks must use tx, never the Store connection pool, and must
// not mutate the supplied independent task snapshots. An error rolls back the
// entire transition, including its lifecycle notification.
type TaskHooks struct {
	Create     func(context.Context, *sql.Tx, *bt.Task) error
	Transition func(context.Context, *sql.Tx, *bt.Task, *bt.Task) error
}

// WithHooks returns an independent provider view. Existing views are unchanged;
// there is no runtime mutation of shared callbacks.
func (s *TaskStore) WithHooks(hooks TaskHooks) *TaskStore {
	copy := *s
	copy.hooks = hooks
	return &copy
}

func cloneTaskSnapshot(task *bt.Task) *bt.Task {
	if task == nil {
		return nil
	}
	out := *task
	out.Spec.Payload = bytes.Clone(task.Spec.Payload)
	out.Checkpoint = bytes.Clone(task.Checkpoint)
	out.ResultData = bytes.Clone(task.ResultData)
	out.PendingResume = bytes.Clone(task.PendingResume)
	out.ContextSnapshot = bytes.Clone(task.ContextSnapshot)
	if task.CancelRequestedAt != nil {
		v := *task.CancelRequestedAt
		out.CancelRequestedAt = &v
	}
	if task.DoneAt != nil {
		v := *task.DoneAt
		out.DoneAt = &v
	}
	return &out
}

func (s *TaskStore) transitionHook(ctx context.Context, tx *sql.Tx, before, after *bt.Task) error {
	if s.hooks.Transition == nil {
		return nil
	}
	return s.hooks.Transition(ctx, tx, cloneTaskSnapshot(before), cloneTaskSnapshot(after))
}

// TaskLeaseTx reads a task and its current lease in the caller's transaction.
// It does not resolve expiry or mutate lifecycle state.
func (s *TaskStore) TaskLeaseTx(ctx context.Context, tx *sql.Tx, id string) (*bt.Task, time.Time, error) {
	r, err := loadTask(ctx, tx, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	return cloneTaskSnapshot(r.task), time.Unix(0, r.lease), nil
}

// CheckAttemptTx fences a side effect in the same SQL transaction as its write.
// allowStopping permits final checkpoint/events after durable stop intent;
// tools must use false before starting new external effects.
func (s *TaskStore) CheckAttemptTx(ctx context.Context, tx *sql.Tx, id string, attempt int64, allowStopping bool) error {
	r, err := loadTask(ctx, tx, id)
	if err != nil {
		return err
	}
	if attempt < 1 || r.task.Attempt != attempt || r.task.Status != bt.StatusRunning || r.lease <= s.now().UnixNano() || (!allowStopping && r.task.CancelRequestedAt != nil) {
		return bt.ErrLeaseLost
	}
	return nil
}
