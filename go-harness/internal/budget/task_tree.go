package budget

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// TaskTreeRootStopped is read-only. The runtime's existing lease monitor uses
// it to stop exactly this root's I/O, never a newer run sharing its session.
func (l *Ledger) TaskTreeRootStopped(ctx context.Context, root string) error {
	var present bool
	if err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_task_tree_stops')`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	var stopped bool
	if err := l.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_task_tree_stops WHERE root_id=? AND node_id='')`, root).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return harness.ErrTaskTreeStopped
	}
	return nil
}
