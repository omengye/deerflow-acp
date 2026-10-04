package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func recordIntentRole(ctx context.Context, tx *sql.Tx, root, intent string) error {
	p, ok := interaction.PermissionProvenanceFromContext(ctx)
	if !ok {
		return nil
	}
	if p.RootID != root || p.RoleID == "" {
		return harness.ErrExecutionConflict
	}
	if _, err := agents.ReadInvocation(ctx, tx, p.RoleID, root); err != nil {
		return err
	}
	if err := agents.CheckStopped(ctx, tx, root, p.RoleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO harness_execution_intent_roles(intent_id,root_id,invocation_id,operation_id) VALUES(?,?,?,?) ON CONFLICT(intent_id) DO NOTHING`, intent, root, p.RoleID, p.OperationID); err != nil {
		return err
	}
	var saved interaction.PermissionProvenance
	if err := tx.QueryRowContext(ctx, `SELECT root_id,invocation_id,operation_id FROM harness_execution_intent_roles WHERE intent_id=?`, intent).Scan(&saved.RootID, &saved.RoleID, &saved.OperationID); err != nil {
		return err
	}
	if saved != p {
		return harness.ErrExecutionConflict
	}
	return nil
}

func readIntentRole(ctx context.Context, tx *sql.Tx, root, intent string) (interaction.PermissionProvenance, error) {
	var p interaction.PermissionProvenance
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_execution_intent_roles')`).Scan(&exists); err != nil {
		return p, err
	}
	if !exists {
		return p, nil
	}
	err := tx.QueryRowContext(ctx, `SELECT root_id,invocation_id,operation_id FROM harness_execution_intent_roles WHERE intent_id=?`, intent).Scan(&p.RootID, &p.RoleID, &p.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if p.RootID != root || p.RoleID == "" {
		return p, harness.ErrExecutionConflict
	}
	return p, nil
}

func stoppedIntentRole(ctx context.Context, tx *sql.Tx, root, intent string) (string, error) {
	p, err := readIntentRole(ctx, tx, root, intent)
	if err != nil || p.RoleID == "" {
		return "", err
	}
	if err = agents.CheckStoppedIfConfigured(ctx, tx, root, p.RoleID); errors.Is(err, agents.ErrStopped) {
		return p.RoleID, nil
	}
	return "", err
}

// CancelStoppedRole runs only in a freshly claimed foreground attempt. The
// native role has not restarted. Retire pending effects as not_executed, then
// let its parent consume a cancellation result. Never synthesize a denial or
// consume an approval grant for the stopped branch.
func (a *executionAttempt) CancelStoppedRole(ctx context.Context, id string) (err error) {
	defer func() {
		if err != nil {
			err = &runPersistenceError{err: err}
		}
	}()
	if err = a.service.Coordinator.Authorize(a.lease.Scope.SessionID, a.lease.OwnerID); err != nil {
		return err
	}
	return withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, a.lease)
		if err != nil {
			return err
		}
		if err = agents.CheckStopped(ctx, tx, row.RootBudgetID, id); !errors.Is(err, agents.ErrStopped) {
			if err == nil {
				return harness.ErrExecutionConflict
			}
			return err
		}
		var active bool
		if err = tx.QueryRowContext(ctx, treeDescendants+`SELECT EXISTS(SELECT 1 FROM harness_agent_attempts WHERE invocation_id IN (SELECT id FROM roles) AND state='active')`, row.RootBudgetID, id, id, row.RootBudgetID).Scan(&active); err != nil {
			return err
		}
		if active {
			return harness.ErrExecutionConflict
		}
		rows, err := tx.QueryContext(ctx, treeDescendants+`SELECT r.intent_id,r.operation_id,i.tool_call_id FROM harness_execution_intent_roles r JOIN harness_execution_intents i ON i.id=r.intent_id WHERE r.root_id=? AND r.invocation_id IN (SELECT id FROM roles) AND i.run_id=? AND i.state='pending'`, row.RootBudgetID, id, id, row.RootBudgetID, row.RootBudgetID, row.State.RunID)
		if err != nil {
			return err
		}
		type pending struct{ intent, operation, call string }
		var items []pending
		for rows.Next() {
			var p pending
			if err = rows.Scan(&p.intent, &p.operation, &p.call); err != nil {
				break
			}
			items = append(items, p)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range items {
			receipt, err := readReceipt(ctx, tx, row.State.SessionID, row.State.RunID, p.call)
			if err != nil {
				return err
			}
			if receipt.State != harness.ReceiptPending {
				return harness.ErrReceiptConflict
			}
			if p.operation != "" {
				if err = a.service.Store.BudgetLedger.CancelPausedToolTx(ctx, tx, a.lease.Scope, p.operation); err != nil {
					return err
				}
			}
			receipt.State = harness.ReceiptNotExecuted
			receipt.Version++
			receipt.UpdatedAt = time.Now().UTC()
			receipt.Error = "delegated task cancelled before tool dispatch"
			if err = writeReceipt(ctx, tx, receipt); err != nil {
				return err
			}
			if _, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: receipt.SessionID, RunID: receipt.RunID, Kind: "tool_end", ToolCallID: receipt.ToolCallID, ToolName: receipt.ToolName, Status: "failed", Content: []harness.Content{{Type: "text", Text: receipt.Error}}, Receipt: &receipt}); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE id=? AND state='pending'`, timestamp(), p.intent); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_grants SET state='revoked' WHERE intent_id=? AND state='ready'`, p.intent); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, treeDescendants+`UPDATE harness_agent_states SET state='cancelled',version=version+1,result_text='',result_available=0,result_truncated=0,updated_at=? WHERE invocation_id IN (SELECT id FROM roles) AND state IN ('waiting_input','cancelling')`, row.RootBudgetID, id, id, row.RootBudgetID, timestamp())
		return err
	})
}
