package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

// ResolvePolicyPermission creates a one-use grant from the pinned deployment
// policy. It never impersonates a human actor or reuses a connection approval.
// Dispatch/denial still consumes the grant in the tool receipt transaction.
func (a *BackgroundInteractionAttempt) ResolvePolicyPermission(ctx context.Context, p harness.PermissionRequest, intent interaction.PermissionIntent) (decision harness.PermissionDecision, handled bool, err error) {
	err = withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		req, err := a.store.bindingTx(ctx, tx, a.scope.Binding, true)
		if err != nil {
			return err
		}
		decision, handled = configuredPermission(req.Session, p)
		if !handled {
			return nil
		}
		task, err := readBackgroundNativeTask(ctx, tx, a.scope.Binding.TaskID)
		if err != nil {
			return err
		}
		policySHA := executionConfigDigest(req.Session)
		var grantID, source string
		var existingDecision harness.PermissionDecision
		err = tx.QueryRowContext(ctx, `SELECT g.id,g.decision,COALESCE(p.policy_sha,'') FROM harness_background_permission_grants g LEFT JOIN harness_background_policy_grants p ON p.grant_id=g.id WHERE g.task_id=? AND g.attempt=? AND g.intent_id=?`, a.scope.Binding.TaskID, a.scope.Attempt, intent.ID).Scan(&grantID, &existingDecision, &source)
		if errors.Is(err, sql.ErrNoRows) {
			grantID = NewID()
			// Empty approved_by denotes no human approver. The separate policy
			// audit row is authoritative and commits with the grant.
			if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_permission_grants(id,task_id,attempt,intent_id,intent_version,decision,state,approved_by,task_version) VALUES(?,?,?,?,?,?,'ready','',?)`, grantID, a.scope.Binding.TaskID, a.scope.Attempt, intent.ID, intent.Version, decision, task.Version); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_policy_grants(grant_id,policy_sha,config_version) VALUES(?,?,?)`, grantID, policySHA, req.Session.ConfigVersion); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if source != policySHA || existingDecision != decision {
			return harness.ErrExecutionConflict
		}
		_, err = a.validateGrantTx(ctx, tx, p, intent, grantID, false)
		return err
	})
	return
}

var _ interaction.PolicyPermissionBroker = (*BackgroundInteractionAttempt)(nil)
