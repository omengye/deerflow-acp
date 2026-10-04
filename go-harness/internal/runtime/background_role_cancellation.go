package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func recordBackgroundIntentRole(ctx context.Context, tx *sql.Tx, b background.Binding, intent string) (string, error) {
	p, ok := interaction.PermissionProvenanceFromContext(ctx)
	if !ok {
		return "", nil
	}
	if p.RootID != b.RootBudgetID || p.RoleID == "" {
		return "", harness.ErrExecutionConflict
	}
	allowed, err := agents.IsDescendant(ctx, tx, b.RootBudgetID, b.RoleID(), p.RoleID)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", harness.ErrPermissionDenied
	}
	if err = agents.CheckStopped(ctx, tx, b.RootBudgetID, p.RoleID); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_intent_roles(intent_id,root_id,invocation_id,operation_id) VALUES(?,?,?,?) ON CONFLICT(intent_id) DO NOTHING`, intent, p.RootID, p.RoleID, p.OperationID)
	if err != nil {
		return "", err
	}
	saved, err := readBackgroundIntentRole(ctx, tx, b.RootBudgetID, intent)
	if err != nil {
		return "", err
	}
	if saved != p {
		return "", harness.ErrExecutionConflict
	}
	return p.RoleID, nil
}

func readBackgroundIntentRole(ctx context.Context, tx *sql.Tx, root, intent string) (interaction.PermissionProvenance, error) {
	var p interaction.PermissionProvenance
	err := tx.QueryRowContext(ctx, `SELECT root_id,invocation_id,operation_id FROM harness_background_intent_roles WHERE intent_id=?`, intent).Scan(&p.RootID, &p.RoleID, &p.OperationID)
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

func stoppedBackgroundIntentRole(ctx context.Context, tx *sql.Tx, root, intent string) (string, error) {
	p, err := readBackgroundIntentRole(ctx, tx, root, intent)
	if err != nil || p.RoleID == "" {
		return "", err
	}
	if err = agents.CheckStoppedIfConfigured(ctx, tx, root, p.RoleID); errors.Is(err, agents.ErrStopped) {
		return p.RoleID, nil
	}
	return "", err
}

// The effective approval ID is derived from committed, immutable evidence.
// Repeated reads and daemon restarts return the same ID; another subtree stop
// changes the ID and invalidates an approval still in flight for the old batch.
func backgroundActiveManifest(ctx context.Context, tx *sql.Tx, m backgroundPermissionManifest) (backgroundPermissionManifest, map[string]interaction.RoleCancellationResume, error) {
	visible := m
	visible.Interrupts = nil
	cancelled := map[string]interaction.RoleCancellationResume{}
	var identities []string
	for _, b := range m.Interrupts {
		role, err := stoppedBackgroundIntentRole(ctx, tx, m.Binding.RootBudgetID, b.IntentID)
		if err != nil {
			return visible, nil, err
		}
		if role == "" {
			visible.Interrupts = append(visible.Interrupts, b)
			continue
		}
		if role != b.RoleID {
			return visible, nil, harness.ErrExecutionConflict
		}
		cancelled[b.NativeInterruptID] = interaction.RoleCancellationResume{IntentID: b.IntentID, IntentVersion: b.IntentVersion, RoleID: role}
		identities = append(identities, fmt.Sprintf("%s/%d/%s", b.IntentID, b.IntentVersion, role))
	}
	if len(identities) > 0 {
		sort.Strings(identities)
		data, _ := json.Marshal(struct {
			Manifest string
			Stopped  []string
		}{m.ID, identities})
		visible.ID = fmt.Sprintf("role-stop/%x", sha256.Sum256(data))
	}
	return visible, cancelled, nil
}

type backgroundResumeTargets struct {
	Permissions   map[string]interaction.PermissionResume
	Cancellations map[string]interaction.RoleCancellationResume
}

func (t backgroundResumeTargets) encoded() ([]byte, error) {
	all := map[string]any{}
	for id, p := range t.Permissions {
		all[id] = p
	}
	for id, p := range t.Cancellations {
		if _, exists := all[id]; exists {
			return nil, harness.ErrExecutionConflict
		}
		all[id] = p
	}
	return json.Marshal(all)
}

func (a *BackgroundInteractionAttempt) CancelStoppedRole(ctx context.Context, id string) (err error) {
	defer func() {
		if err != nil {
			err = &runPersistenceError{err: err}
		}
	}()
	return withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		b := a.scope.Binding
		allowed, err := agents.IsDescendant(ctx, tx, b.RootBudgetID, b.RoleID(), id)
		if err != nil {
			return err
		}
		if !allowed || id == b.RoleID() {
			return harness.ErrPermissionDenied
		}
		if err = agents.CheckStopped(ctx, tx, b.RootBudgetID, id); !errors.Is(err, agents.ErrStopped) {
			if err == nil {
				return harness.ErrExecutionConflict
			}
			return err
		}
		var active bool
		if err = tx.QueryRowContext(ctx, treeDescendants+`SELECT EXISTS(SELECT 1 FROM harness_agent_attempts WHERE invocation_id IN (SELECT id FROM roles) AND state='active')`, b.RootBudgetID, id, id, b.RootBudgetID).Scan(&active); err != nil {
			return err
		}
		if active {
			return harness.ErrExecutionConflict
		}
		rows, err := tx.QueryContext(ctx, treeDescendants+`SELECT r.intent_id,r.operation_id,i.tool_call_id FROM harness_background_intent_roles r JOIN harness_background_permission_intents i ON i.id=r.intent_id WHERE r.root_id=? AND r.invocation_id IN (SELECT id FROM roles) AND i.task_id=? AND i.state='pending'`, b.RootBudgetID, id, id, b.RootBudgetID, b.RootBudgetID, b.TaskID)
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
			receipt, err := readReceipt(ctx, tx, b.ChildSessionID, b.TaskID, p.call)
			if err != nil {
				return err
			}
			if receipt.State != harness.ReceiptPending {
				return harness.ErrReceiptConflict
			}
			if p.operation != "" {
				if err = a.store.store.BudgetLedger.CancelPausedToolTx(ctx, tx, background.BudgetScope(a.scope), p.operation); err != nil {
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
			if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_intents SET state='cancelled',version=version+1 WHERE id=? AND state='pending'`, p.intent); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_grants SET state='revoked' WHERE intent_id=? AND state='ready'`, p.intent); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, treeDescendants+`UPDATE harness_agent_states SET state='cancelled',version=version+1,result_text='',result_available=0,result_truncated=0,updated_at=? WHERE invocation_id IN (SELECT id FROM roles) AND state IN ('waiting_input','cancelling')`, b.RootBudgetID, id, id, b.RootBudgetID, timestamp())
		return err
	})
}
