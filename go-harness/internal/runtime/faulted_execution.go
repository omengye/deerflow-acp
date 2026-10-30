package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// CloseFaultedExecutionRequest targets one legacy execution. It is an explicit
// host recovery operation, never a startup migration or a model tool.
type CloseFaultedExecutionRequest struct {
	RunID           string
	ExpectedVersion int64
}

// CloseFaultedExecution ends an idle legacy fault without asserting that its
// tool effects succeeded or had no effect. The next prompt starts a new run.
func (s *Service) CloseFaultedExecution(ctx context.Context, owner, sessionID string, request CloseFaultedExecutionRequest) (harness.ExecutionState, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return harness.ExecutionState{}, err
	}
	defer release()
	var state harness.ExecutionState
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		state, err = s.Store.closeFaultedExecutionTx(ctx, tx, sessionID, owner, request)
		return err
	})
	return state, err
}

func (s *Store) closeFaultedExecutionTx(ctx context.Context, tx *sql.Tx, sessionID, owner string, request CloseFaultedExecutionRequest) (harness.ExecutionState, error) {
	if sessionID == "" || owner == "" || request.RunID == "" || request.ExpectedVersion < 1 {
		return harness.ExecutionState{}, harness.ErrInvalidInput
	}
	row, err := readExecution(ctx, tx, sessionID, request.RunID)
	if err != nil {
		return row.State, err
	}
	if row.State.Status != harness.ExecutionNeedsReconciliation || row.State.Version != request.ExpectedVersion {
		return row.State, harness.ErrExecutionConflict
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM harness_execution_attempts WHERE run_id=? AND status='running') OR
 EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state IN ('active','unknown')) OR
 EXISTS(SELECT 1 FROM budget_reservations WHERE member_id=? AND state IN ('reserved','dispatched','unknown'))`, request.RunID, request.RunID, request.RunID).Scan(&active); err != nil {
		return row.State, err
	}
	if active {
		return row.State, harness.ErrBusy
	}
	if err = stopAutomaticRootTx(ctx, tx, row); err != nil {
		return row.State, err
	}
	if err = rejectPendingSteeringTx(ctx, tx, request.RunID, "faulted_execution_closed"); err != nil {
		return row.State, err
	}
	if err = settleOpenReceipts(ctx, tx, request.RunID, "legacy execution closed without replay"); err != nil {
		return row.State, err
	}
	if err = revokeExecutionGrants(ctx, tx, row.State.AttemptID); err != nil {
		return row.State, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE run_id=? AND state='pending'`, now, request.RunID); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+request.RunID); err != nil {
		return row.State, err
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_executions SET status='failed',version=version+1,updated_at=? WHERE run_id=? AND version=? AND status='needs_reconciliation'`, now, request.RunID, request.ExpectedVersion); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='failed',stop_reason='failed',updated_at=? WHERE id=? AND session_id=?`, now, request.RunID, sessionID); err != nil {
		return row.State, err
	}
	lease := ExecutionLease{OwnerID: owner, InputID: row.State.InputID, Scope: budget.Scope{RootBudgetID: row.RootBudgetID, MemberID: row.State.RunID, SessionID: row.State.SessionID, AttemptID: row.State.AttemptID, Fence: row.State.Attempt}}
	if err = executionAudit(ctx, tx, lease, "faulted_execution_closed", map[string]any{"previousStatus": row.State.Status, "effectsVerified": false, "automaticReplay": false}); err != nil {
		return row.State, err
	}
	row, err = readExecution(ctx, tx, sessionID, request.RunID)
	return row.State, err
}
