package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const treeDescendants = `WITH RECURSIVE roles(id) AS (SELECT id FROM harness_agent_invocations WHERE root_id=? AND (?='' OR id=?) UNION SELECT i.id FROM harness_agent_invocations i JOIN roles p ON i.parent_id=p.id WHERE i.root_id=?) `

// CancelTaskTree commits a stop before delivering cancellation. Waiting on
// joined cleanup is bounded; a response may honestly remain cancelling. The
// fence remains durable when the caller disconnects or the daemon restarts.
func (s *Service) CancelTaskTree(ctx context.Context, actor harness.TaskActor, req harness.TaskTreeCancelRequest) (harness.TaskTreeNode, error) {
	var out harness.TaskTreeNode
	if !treeIdentity(req.RootID) || !treeIdentity(req.NodeID) || req.ExpectedVersion < 1 {
		return out, harness.ErrInvalidInput
	}
	if err := s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return out, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE budget_roots SET revision=revision WHERE id=? AND session_id=?`, req.RootID, actor.SessionID); err != nil {
		return out, err
	}
	node, err := s.Store.TaskTreeNodeTx(ctx, tx, actor.SessionID, req.RootID, req.NodeID)
	if err != nil {
		return out, err
	}
	selected := node.InvocationID
	if node.Mode == "root" {
		selected = ""
	}
	var stopped bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_task_tree_stops WHERE root_id=? AND node_id=?)`, req.RootID, selected).Scan(&stopped); err != nil {
		return out, err
	}
	if !stopped && node.Version != req.ExpectedVersion {
		return out, harness.ErrExecutionConflict
	}
	if !stopped {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_task_tree_stops(root_id,node_id,created_at) VALUES(?,?,?)`, req.RootID, selected, timestamp()); err != nil {
			return out, err
		}
		if _, err = tx.ExecContext(ctx, treeDescendants+`UPDATE harness_agent_states SET state='cancelling',version=version+1,updated_at=? WHERE invocation_id IN (SELECT id FROM roles) AND state IN ('running','waiting_input')`, req.RootID, selected, selected, req.RootID, timestamp()); err != nil {
			return out, err
		}
		// A reverse permission response already in flight cannot claim the old
		// execution version after its selected logical branch has been stopped.
		if selected != "" {
			if _, err = tx.ExecContext(ctx, treeDescendants+`UPDATE harness_executions SET version=version+1,updated_at=? WHERE root_budget_id=? AND session_id=? AND status='waiting_input' AND run_id IN (SELECT i.run_id FROM harness_execution_intents i JOIN harness_execution_intent_roles r ON r.intent_id=i.id WHERE r.invocation_id IN (SELECT id FROM roles) AND i.state='pending')`, req.RootID, selected, selected, req.RootID, timestamp(), req.RootID, actor.SessionID); err != nil {
				return out, err
			}
		}
	}
	// Match normalized logical IDs, including newer continuation tasks. Parent
	// and root registration checks prevent a later descendant escaping the set.
	tables, err := treeTables(ctx, tx)
	if err != nil {
		return out, err
	}
	var ids []string
	if tables["harness_background_bindings"] {
		if tables["harness_background_intent_roles"] {
			if _, err = tx.ExecContext(ctx, treeDescendants+`UPDATE harness_background_permission_grants SET state='revoked' WHERE state='ready' AND intent_id IN (SELECT r.intent_id FROM harness_background_intent_roles r WHERE r.root_id=? AND (?='' OR r.invocation_id IN (SELECT id FROM roles)))`, req.RootID, selected, selected, req.RootID, req.RootID, selected); err != nil {
				return out, err
			}
		}
		logicalID := `COALESCE(NULLIF(json_extract(CAST(b.payload AS TEXT),'$.InvocationID'),''),'background/'||b.task_id)`
		filter := `json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID')=? AND (?='' OR ` + logicalID + `=? OR ` + logicalID + ` IN (SELECT id FROM roles))`
		args := []any{req.RootID, selected, selected, req.RootID, "task tree cancellation", timestamp(), req.RootID, selected, selected}
		if _, err = tx.ExecContext(ctx, treeDescendants+`INSERT INTO harness_background_stops(task_id,reason,created_at) SELECT b.task_id,?,? FROM harness_background_bindings b WHERE `+filter+` ON CONFLICT(task_id) DO NOTHING`, args...); err != nil {
			return out, err
		}
		rows, e := tx.QueryContext(ctx, treeDescendants+`SELECT b.task_id FROM harness_background_bindings b JOIN eino_background_tasks t ON t.id=b.task_id WHERE `+filter+` AND t.status NOT IN ('completed','failed','canceled') ORDER BY b.task_id LIMIT 100`, req.RootID, selected, selected, req.RootID, req.RootID, selected, selected)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	if selected == "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_automatic_stops(root_id,session_id,reason,created_at) VALUES(?,?,'task tree cancelled',?) ON CONFLICT(root_id) DO NOTHING`, req.RootID, actor.SessionID, timestamp()); err != nil {
			return out, err
		}
		rows, e := tx.QueryContext(ctx, `SELECT run_id,version FROM harness_executions WHERE root_budget_id=? AND session_id=? AND status IN ('waiting_input','needs_reconciliation')`, req.RootID, actor.SessionID)
		if e != nil {
			return out, e
		}
		var requests []harness.CancelExecutionRequest
		for rows.Next() {
			var r harness.CancelExecutionRequest
			if e = rows.Scan(&r.RunID, &r.ExpectedVersion); e != nil {
				break
			}
			requests = append(requests, r)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return out, e
		}
		for _, r := range requests {
			if _, err = s.Store.CancelExecutionTx(ctx, tx, actor.SessionID, actor.OwnerID, r); err != nil {
				return out, err
			}
		}
	}
	if err = s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	// Native workers also scan every durable stop, so an interrupted delivery
	// or a very large subtree remains stopped without a successful RPC retry.
	if s.Background != nil {
		for _, id := range ids {
			if _, e := s.Background.CancelBackgroundTask(ctx, actor, id); e != nil && !errors.Is(e, harness.ErrExecutionConflict) {
				return out, e
			}
		}
	}
	join, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err = s.TaskTreeNode(ctx, actor, req.RootID, req.NodeID)
		if err != nil {
			return out, err
		}
		var active bool
		err = s.Store.db.QueryRowContext(ctx, treeDescendants+`SELECT EXISTS(SELECT 1 FROM harness_agent_attempts a WHERE a.invocation_id IN (SELECT id FROM roles) AND a.state='active') OR EXISTS(SELECT 1 FROM budget_attempts a WHERE ?='' AND a.root_id=? AND a.state='active')`, req.RootID, selected, selected, req.RootID, selected, req.RootID).Scan(&active)
		if err != nil {
			return out, err
		}
		if !active {
			return out, nil
		}
		select {
		case <-join.Done():
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, nil
		case <-ticker.C:
		}
	}
}
