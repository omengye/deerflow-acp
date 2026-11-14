package runtime

import (
	"context"
	"database/sql"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

var errFailedTaskWait = errors.New("a delegated task ended without a safely deliverable result; the waiting turn has ended and a new message can continue the conversation")

// A terminal dependency may have an unconfirmed result without having live I/O.
// Ending its waiter does not consume that result or permit replay of its tools.
func terminalTaskWaitFailure(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var status, child, blocked string
	var lease int64
	err := tx.QueryRowContext(ctx, `SELECT t.status,t.lease_expires_at,b.child_session_id,b.blocked_reason FROM eino_background_tasks t JOIN harness_background_bindings b ON b.task_id=t.id WHERE t.id=?`, taskID).Scan(&status, &lease, &child, &blocked)
	if err != nil || !terminalBackgroundStatus(status) || lease != 0 {
		return false, err
	}
	var live, unsafe bool
	err = tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM harness_background_child_leases WHERE task_id=?) OR
 EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state='started') OR
 EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state='active'),
 EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND `+unmanagedUncertainReceiptSQL("")+`) OR
 EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state='unknown') OR
 EXISTS(SELECT 1 FROM budget_reservations WHERE member_id=? AND state='unknown')`, taskID, child, taskID, child, taskID, taskID).Scan(&live, &unsafe)
	if err != nil || live {
		return false, err
	}
	var failureTable bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_background_execution_failures')`).Scan(&failureTable); err != nil {
		return false, err
	}
	if failureTable {
		var persistenceFailure bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_execution_failures WHERE task_id=? AND persistence_failure<>0)`, taskID).Scan(&persistenceFailure); err != nil {
			return false, err
		}
		unsafe = unsafe || persistenceFailure
	}
	return blocked != "" || unsafe, nil
}

func (s *Store) closeFailedTaskWaitTx(ctx context.Context, tx *sql.Tx, owner, sessionID string, explicitPrompt bool) (*harness.ExecutionState, error) {
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT run_id FROM harness_executions WHERE session_id=? AND status='waiting_input'`, sessionID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row, err := readExecution(ctx, tx, sessionID, runID)
	if err != nil || row.Manifest == nil || len(row.Manifest.TaskWaits) == 0 {
		return nil, err
	}
	if !explicitPrompt && row.Manifest.AgentRecoveryVersion != 1 {
		return nil, nil
	}
	// Verify stored wait ownership and checkpoint identity before using task IDs.
	if err = validateExecutionManifest(ctx, tx, row); err != nil {
		if errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrExecutionConflict) || errors.Is(err, harness.ErrReconciliationRequired) {
			return nil, nil
		}
		return nil, err
	}
	var live bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_execution_attempts WHERE run_id=? AND status='running') OR EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state='active') OR EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=? AND state='started')`, runID, runID, runID).Scan(&live); err != nil || live {
		return nil, err
	}
	failed := ""
	for _, wait := range row.Manifest.TaskWaits {
		unsafe, err := terminalTaskWaitFailure(ctx, tx, wait.Wait.TaskID)
		if err != nil {
			return nil, err
		}
		if unsafe {
			failed = wait.Wait.TaskID
			break
		}
	}
	if failed == "" {
		return nil, nil
	}
	if err = stopAutomaticRootTx(ctx, tx, row); err != nil {
		return nil, err
	}
	if err = rejectPendingSteeringTx(ctx, tx, runID, "dependency_failed"); err != nil {
		return nil, err
	}
	if err = settleOpenReceipts(ctx, tx, runID, "waiting execution ended after a failed dependency"); err != nil {
		return nil, err
	}
	if err = revokeExecutionGrants(ctx, tx, row.State.AttemptID); err != nil {
		return nil, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE run_id=? AND state='pending'`, now, runID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+runID); err != nil {
		return nil, err
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_executions SET status='failed',version=version+1,blocked_reason=?,updated_at=? WHERE run_id=? AND version=? AND status='waiting_input'`, errFailedTaskWait.Error(), now, runID, row.State.Version); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='failed',stop_reason='failed',error=?,updated_at=? WHERE id=?`, errFailedTaskWait.Error(), now, runID); err != nil {
		return nil, err
	}
	lease := ExecutionLease{OwnerID: owner, InputID: row.State.InputID, Scope: budget.Scope{RootBudgetID: row.RootBudgetID, MemberID: runID, SessionID: sessionID, AttemptID: row.State.AttemptID, Fence: row.State.Attempt}}
	if err = executionAudit(ctx, tx, lease, "dependency_wait_failed", map[string]any{"taskId": failed, "effectsVerified": false, "automaticReplay": false}); err != nil {
		return nil, err
	}
	row, err = readExecution(ctx, tx, sessionID, runID)
	return &row.State, err
}

func (s *Service) closeFailedTaskWait(ctx context.Context, owner, sessionID string) (*harness.ExecutionState, error) {
	ctx, release, err := s.Coordinator.BeginMaintenance(ctx, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	var state *harness.ExecutionState
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		state, err = s.Store.closeFailedTaskWaitTx(ctx, tx, owner, sessionID, false)
		return err
	})
	return state, err
}

func (s *BackgroundInteractionStore) closeFailedBackgroundWaitTx(ctx context.Context, tx *sql.Tx, m backgroundWaitManifest, task *bt.Task) (bool, error) {
	if m.AgentRecoveryVersion != 1 || task.Status != bt.StatusWaitingInput || task.Version != m.Frontier.TaskVersion {
		return false, nil
	}
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_child_leases WHERE task_id=?) OR EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state='active') OR EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=? AND state='started')`, task.Spec.ID, task.Spec.ID, task.Spec.ID).Scan(&live); err != nil || live {
		return false, err
	}
	failed := false
	for _, wait := range m.Waits {
		unsafe, err := terminalTaskWaitFailure(ctx, tx, wait.Wait.TaskID)
		if err != nil {
			return false, err
		}
		failed = failed || unsafe
	}
	if !failed {
		return false, nil
	}
	if err := s.validateFrontier(ctx, tx, m.Frontier, task); err != nil {
		return false, err
	}
	before, after, err := sqlite.FailWaitingTaskTx(ctx, tx, task.Spec.ID, task.Version, errFailedTaskWait.Error())
	if err != nil {
		return false, err
	}
	if err = settleOpenReceipts(ctx, tx, task.Spec.ID, "waiting background task ended after a failed dependency"); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM eino_checkpoints WHERE id=?`, task.Spec.ID+"/checkpoint"); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='failed',stop_reason='failed',error=?,updated_at=? WHERE id=?`, errFailedTaskWait.Error(), timestamp(), task.Spec.ID); err != nil {
		return false, err
	}
	if err = s.TransitionTx(ctx, tx, background.TaskScope{Binding: m.Frontier.Binding, Attempt: task.Attempt}, before, after); err != nil {
		return false, err
	}
	return true, nil
}
