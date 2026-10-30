package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

// FailWaitingTaskTx terminates an idle dependency wait without starting another
// attempt or replaying its checkpoint. The caller owns tx and must roll it back
// on any error. It also owns business projections/transition hooks in the same
// transaction, using the returned independent before/after snapshots.
// The native provider has no separate attempt row: only running tasks with a
// lease authorize attempt work. Runtime-owned child leases and budgets must be
// checked by the caller before this transition.
func FailWaitingTaskTx(ctx context.Context, tx *sql.Tx, taskID string, expectedVersion int64, reason string) (before, after *bt.Task, err error) {
	if tx == nil || taskID == "" || expectedVersion < 1 || reason == "" || len(reason) > 4096 {
		return nil, nil, errors.New("backgroundtask: waiting failure requires a transaction, task identity, version and 1 to 4096 byte reason")
	}
	r, err := loadTask(ctx, tx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if err = checkVersion(r, expectedVersion); err != nil {
		return nil, nil, err
	}
	if terminal(r.task.Status) {
		return nil, nil, bt.ErrAlreadyTerminal
	}
	if r.task.Status != bt.StatusWaitingInput || r.task.DoneAt != nil || r.task.CancelRequestedAt != nil {
		return nil, nil, bt.ErrIllegalTransition
	}
	if r.lease != 0 {
		return nil, nil, bt.ErrLeaseLost
	}
	// Do not let a corrupt/stale payload hide an active SQL lifecycle state.
	var status string
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT status,version FROM eino_background_tasks WHERE id=?`, taskID).Scan(&status, &version); err != nil {
		return nil, nil, err
	}
	if status != string(bt.StatusWaitingInput) || version != expectedVersion {
		return nil, nil, bt.ErrVersionConflict
	}
	before = cloneTaskSnapshot(r.task)
	r.task.Status = bt.StatusFailed
	r.task.ResultError = reason
	r.task.ResultData = nil
	r.task.Checkpoint = nil
	provider := &TaskStore{now: time.Now}
	provider.finish(r)
	if err = saveTask(ctx, tx, r, expectedVersion); err != nil {
		return before, nil, err
	}
	if err = provider.enqueueLifecycle(ctx, tx, r.task, bt.NotificationFailed); err != nil {
		return before, nil, err
	}
	return before, cloneTaskSnapshot(r.task), nil
}
