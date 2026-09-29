package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// EndExecutionAttemptTx is called after actual model/tool I/O and cleanup have
// joined. Ledger EndAttemptTx must be in this transaction as well. Failed retry
// spending remains in that ledger even when the original checkpoint is kept.
func (s *Store) EndExecutionAttemptTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, kind harness.ExecutionStopKind, detail string) (harness.ExecutionState, error) {
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return row.State, err
	}
	status := harness.ExecutionFailed
	blocked := ""
	switch kind {
	case harness.ExecutionStopCompleted:
		var open bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=? AND state IN ('pending','started','uncertain'))`, row.State.RunID).Scan(&open); err != nil {
			return row.State, err
		}
		if open {
			return row.State, harness.ErrReceiptConflict
		}
		status = harness.ExecutionCompleted
	case harness.ExecutionStopCancelled:
		status = harness.ExecutionCancelled
	case harness.ExecutionStopFailed, harness.ExecutionStopDisconnected:
		validation := validateExecutionManifest(ctx, tx, row)
		if validation == nil {
			status = harness.ExecutionWaitingInput
		} else if errors.Is(validation, harness.ErrExecutionUnresumable) || errors.Is(validation, harness.ErrReconciliationRequired) || errors.Is(validation, harness.ErrExecutionConflict) {
			status = harness.ExecutionNeedsReconciliation
			blocked = validation.Error()
		} else {
			return row.State, validation
		}
	default:
		return row.State, executionInvalid("invalid execution attempt outcome")
	}
	if status != harness.ExecutionWaitingInput {
		if err = settleOpenReceipts(ctx, tx, row.State.RunID, "execution attempt ended before a final tool receipt"); err != nil {
			return row.State, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE run_id=? AND state='pending'`, timestamp(), row.State.RunID); err != nil {
			return row.State, err
		}
	}
	if status == harness.ExecutionCompleted || status == harness.ExecutionCancelled {
		if _, err = tx.ExecContext(ctx, `DELETE FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+row.State.RunID); err != nil {
			return row.State, err
		}
	}
	if err = revokeExecutionGrants(ctx, tx, lease.Scope.AttemptID); err != nil {
		return row.State, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_attempts SET status=?,finished_at=? WHERE id=?`, kind, now, lease.Scope.AttemptID); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_executions SET status=?,version=version+1,blocked_reason=?,updated_at=? WHERE run_id=?`, status, blocked, now, row.State.RunID); err != nil {
		return row.State, err
	}
	stopReason := string(kind)
	if status == harness.ExecutionWaitingInput {
		stopReason = ""
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status=?,stop_reason=?,error=?,updated_at=? WHERE id=?`, status, stopReason, truncateUTF8(detail, 4096), now, row.State.RunID); err != nil {
		return row.State, err
	}
	if err = executionAudit(ctx, tx, lease, "attempt_ended", map[string]any{"reason": kind, "status": status, "detail": truncateUTF8(detail, 4096)}); err != nil {
		return row.State, err
	}
	row, err = readExecution(ctx, tx, row.State.SessionID, row.State.RunID)
	row.State.Resumable = status == harness.ExecutionWaitingInput && err == nil
	return row.State, err
}

// CancelExecutionTx closes an idle waiting/recovery state. Running attempts
// must instead be cancelled through the coordinator and joined before End.
// The caller has already authorized owner using the current coordinator lease.
func (s *Store) CancelExecutionTx(ctx context.Context, tx *sql.Tx, sessionID, owner string, req harness.CancelExecutionRequest) (harness.ExecutionState, error) {
	row, err := readExecution(ctx, tx, sessionID, req.RunID)
	if err != nil {
		return row.State, err
	}
	if owner == "" || req.ExpectedVersion < 1 {
		return row.State, executionInvalid("invalid execution cancellation")
	}
	if row.State.Version != req.ExpectedVersion {
		return row.State, harness.ErrExecutionConflict
	}
	if row.State.Status != harness.ExecutionWaitingInput && row.State.Status != harness.ExecutionNeedsReconciliation {
		return row.State, harness.ErrExecutionConflict
	}
	if err = settleOpenReceipts(ctx, tx, row.State.RunID, "waiting execution explicitly cancelled"); err != nil {
		return row.State, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE run_id=? AND state='pending'`, now, row.State.RunID); err != nil {
		return row.State, err
	}
	if err = revokeExecutionGrants(ctx, tx, row.State.AttemptID); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+row.State.RunID); err != nil {
		return row.State, err
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_executions SET status='cancelled',version=version+1,blocked_reason='',updated_at=? WHERE run_id=? AND version=? AND status IN ('waiting_input','needs_reconciliation')`, now, row.State.RunID, req.ExpectedVersion); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='cancelled',stop_reason='cancelled',error='',updated_at=? WHERE id=?`, now, row.State.RunID); err != nil {
		return row.State, err
	}
	lease := ExecutionLease{OwnerID: owner, InputID: row.State.InputID, Scope: budget.Scope{RootBudgetID: row.RootBudgetID, MemberID: row.State.RunID, SessionID: row.State.SessionID, AttemptID: row.State.AttemptID, Fence: row.State.Attempt}}
	if err = executionAudit(ctx, tx, lease, "execution_cancelled", map[string]any{"owner": owner, "previousStatus": row.State.Status}); err != nil {
		return row.State, err
	}
	row, err = readExecution(ctx, tx, sessionID, req.RunID)
	return row.State, err
}

type ExecutionRecovery struct {
	Scope             budget.Scope
	Status            harness.ExecutionStatus
	AttemptWasRunning bool
}

// RecoverExecutionsTx is the execution-aware startup pass. Run it before the
// legacy reconciliation pass, which must exclude executions preserved here.
// It invokes no model/tool and never infers success from a checkpoint alone.
func (s *Store) RecoverExecutionsTx(ctx context.Context, tx *sql.Tx) ([]ExecutionRecovery, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,run_id FROM harness_executions WHERE status IN ('running','suspending','resuming','waiting_input') ORDER BY run_id`)
	if err != nil {
		return nil, err
	}
	type identity struct{ session, run string }
	var ids []identity
	for rows.Next() {
		var id identity
		if err = rows.Scan(&id.session, &id.run); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]ExecutionRecovery, 0, len(ids))
	for _, id := range ids {
		row, err := readExecution(ctx, tx, id.session, id.run)
		if err != nil {
			return nil, err
		}
		var owner, attemptStatus string
		if err = tx.QueryRowContext(ctx, `SELECT owner_id,status FROM harness_execution_attempts WHERE id=?`, row.State.AttemptID).Scan(&owner, &attemptStatus); err != nil {
			return nil, err
		}
		lease := ExecutionLease{OwnerID: owner, InputID: row.State.InputID, Scope: budget.Scope{RootBudgetID: row.RootBudgetID, MemberID: id.run, SessionID: id.session, AttemptID: row.State.AttemptID, Fence: row.State.Attempt}}
		validation := validateExecutionManifest(ctx, tx, row)
		status, blocked := harness.ExecutionWaitingInput, ""
		if validation != nil {
			if !errors.Is(validation, harness.ErrExecutionUnresumable) && !errors.Is(validation, harness.ErrExecutionConflict) && !errors.Is(validation, harness.ErrReconciliationRequired) {
				return nil, validation
			}
			status, blocked = harness.ExecutionNeedsReconciliation, validation.Error()
			if err = settleOpenReceipts(ctx, tx, id.run, "process stopped without a matching resumable checkpoint"); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_intents SET state='cancelled',version=version+1,updated_at=? WHERE run_id=? AND state='pending'`, timestamp(), id.run); err != nil {
				return nil, err
			}
		}
		if err = revokeExecutionGrants(ctx, tx, lease.Scope.AttemptID); err != nil {
			return nil, err
		}
		if row.State.Status != status || attemptStatus == "running" {
			now := timestamp()
			if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_attempts SET status='interrupted',finished_at=? WHERE id=? AND status='running'`, now, row.State.AttemptID); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_executions SET status=?,version=version+1,blocked_reason=?,updated_at=? WHERE run_id=?`, status, blocked, now, id.run); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status=?,stop_reason='',error=?,updated_at=? WHERE id=?`, status, blocked, now, id.run); err != nil {
				return nil, err
			}
			if err = executionAudit(ctx, tx, lease, "execution_recovered", map[string]any{"status": status, "attemptWasRunning": attemptStatus == "running"}); err != nil {
				return nil, err
			}
		}
		result = append(result, ExecutionRecovery{Scope: lease.Scope, Status: status, AttemptWasRunning: attemptStatus == "running"})
	}
	return result, nil
}

// ExecutionRequestTx reconstructs the original accepted request for a trusted
// claimed attempt. A resume cannot substitute a fresh InputID or prompt.
func (s *Store) ExecutionRequestTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease) (harness.RunRequest, error) {
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return harness.RunRequest{}, err
	}
	if err = validateExecutionConfig(ctx, tx, row); err != nil {
		return harness.RunRequest{}, err
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT content FROM harness_inputs WHERE id=? AND run_id=?`, row.State.InputID, row.State.RunID).Scan(&data); err != nil {
		return harness.RunRequest{}, err
	}
	var input []harness.Content
	if err = jsonUnmarshalExecutionInput(data, &input); err != nil {
		return harness.RunRequest{}, err
	}
	return harness.RunRequest{Session: row.Config, RunID: row.State.RunID, RootBudgetID: row.RootBudgetID, InputID: row.State.InputID, Input: input}, nil
}

func jsonUnmarshalExecutionInput(data []byte, input *[]harness.Content) error {
	if err := json.Unmarshal(data, input); err != nil {
		return fmt.Errorf("decode accepted execution input: %w", err)
	}
	return nil
}
