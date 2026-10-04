package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func treeInvocationUsage(ctx context.Context, tx *sql.Tx, n *harness.TaskTreeNode) error {
	// Mixed pre-upgrade roots cannot reconstruct which old foreground role
	// spent an untagged reservation. Returning a partial total as complete would
	// hide that cost; retain nil for their child usage until a new root starts.
	var unknown bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_reservations r LEFT JOIN budget_operation_owners o ON o.operation_id=r.id WHERE r.root_id=? AND (o.operation_id IS NULL OR o.root_id<>r.root_id))`, n.RootID).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return nil
	}
	u := &harness.TaskTreeUsage{Scope: "invocation"}
	var count int64
	err := tx.QueryRowContext(ctx, `SELECT count(*),
 COALESCE(SUM(CASE WHEN r.kind='model' THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN r.kind='tool' AND NOT EXISTS(SELECT 1 FROM budget_tool_continuations c WHERE c.child_id=r.id) THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN r.kind='model' AND r.state='settled' THEN COALESCE(json_extract(CAST(r.usage_json AS TEXT),'$.inputTokens'),0) ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN r.kind='model' AND r.state='settled' THEN COALESCE(json_extract(CAST(r.usage_json AS TEXT),'$.outputTokens'),0) ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN r.kind='model' AND r.state='settled' THEN COALESCE(json_extract(CAST(r.usage_json AS TEXT),'$.totalTokens'),0) ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN r.kind='model' AND r.state IN ('reserved','dispatched','unknown') THEN r.held_tokens ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN r.kind='model' AND (r.state<>'settled' OR COALESCE(json_extract(CAST(r.usage_json AS TEXT),'$.estimated'),0)=1) THEN 1 ELSE 0 END),0)
 FROM budget_operation_owners o JOIN budget_reservations r ON r.id=o.operation_id
 WHERE o.root_id=? AND o.invocation_id=? AND r.root_id=o.root_id`, n.RootID, n.InvocationID).Scan(&count, &u.ModelCalls, &u.ToolCalls, &u.InputTokens, &u.OutputTokens, &u.TotalTokens, &u.HeldTokens, &u.Estimated)
	if err != nil {
		return err
	}
	if count > 0 {
		n.Usage = u
	}
	return nil
}
