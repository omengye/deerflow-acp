package budget

import (
	"context"
	"database/sql"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// TaskTreeRootStopped is read-only. The runtime's existing lease monitor uses
// it to stop exactly this root's I/O, never a newer run sharing its session.
func (l *Ledger) TaskTreeRootStopped(ctx context.Context, root string) error {
	return taskTreeRootStopped(ctx, l.db, root)
}

// CheckTaskTreeRootStoppedTx also fences saved-result publication, which does
// not reserve another model call and therefore cannot rely on dispatch alone.
func CheckTaskTreeRootStoppedTx(ctx context.Context, tx *sql.Tx, root string) error {
	return taskTreeRootStopped(ctx, tx, root)
}

type taskTreeQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func taskTreeRootStopped(ctx context.Context, q taskTreeQuery, root string) error {
	var present bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_task_tree_stops')`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	var stopped bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_task_tree_stops WHERE root_id=? AND node_id='')`, root).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return harness.ErrTaskTreeStopped
	}
	return nil
}
