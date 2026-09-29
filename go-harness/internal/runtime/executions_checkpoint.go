package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// ExecutionInterruptBinding is emitted by the trusted engine adapter. Public
// resume requests cannot supply or replace these native interruption addresses.
type ExecutionInterruptBinding struct {
	IntentID          string `json:"intentId"`
	IntentVersion     int64  `json:"intentVersion"`
	NativeInterruptID string `json:"nativeInterruptId"`
}
type ExecutionNativeHead struct {
	Sequence int64  `json:"sequence"`
	EventID  string `json:"eventId"`
	Digest   string `json:"digest"`
}
type ExecutionManifest struct {
	Version                                                           int `json:"version"`
	SessionID, RunID, InputID                                         string
	SourceKind, SourceSHA                                             string
	CheckpointID, CheckpointSHA, ConfigSHA, InputSHA, ReceiptFrontier string
	EventCursor                                                       int64
	NativeHead                                                        ExecutionNativeHead
	SummaryHead                                                       ExecutionNativeHead
	Interrupts                                                        []ExecutionInterruptBinding
}
type ExecutionCheckpoint struct {
	ID         string
	Data       []byte
	Interrupts []ExecutionInterruptBinding
}

func executionNativeHead(ctx context.Context, tx *sql.Tx, sessionID string) (ExecutionNativeHead, error) {
	var h ExecutionNativeHead
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT seq,event_id,payload FROM eino_session_events WHERE session_id=? ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&h.Sequence, &h.EventID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	h.Digest = executionDigest(data)
	return h, nil
}
func executionSummaryHead(ctx context.Context, tx *sql.Tx, sessionID string) (ExecutionNativeHead, error) {
	var h ExecutionNativeHead
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT seq,event_id,payload FROM eino_session_events WHERE session_id=? AND kind=? ORDER BY seq DESC LIMIT 1`, sessionID, string(adk.SessionEventMessagesReplaced)).Scan(&h.Sequence, &h.EventID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	h.Digest = executionDigest(data)
	return h, nil
}
func executionEventCursor(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE session_id=?`, sessionID).Scan(&n)
	return n, err
}
func executionReceiptFrontier(ctx context.Context, tx *sql.Tx, sessionID, runID string) (string, error) {
	var unsafe bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND (state='uncertain' OR (run_id=? AND state='started')))`, sessionID, runID).Scan(&unsafe); err != nil {
		return "", err
	}
	if unsafe {
		return "", harness.ErrReconciliationRequired
	}
	rows, err := tx.QueryContext(ctx, `SELECT tool_call_id,state,version,receipt FROM harness_tool_receipts WHERE session_id=? AND run_id=? ORDER BY tool_call_id`, sessionID, runID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type part struct {
		CallID, State string
		Version       int64
		Receipt       json.RawMessage
	}
	parts := []part{}
	for rows.Next() {
		var p part
		if err = rows.Scan(&p.CallID, &p.State, &p.Version, &p.Receipt); err != nil {
			return "", err
		}
		parts = append(parts, p)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	data, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	return executionDigest(data), nil
}
func checkExecutionBindings(ctx context.Context, tx *sql.Tx, runID string, bindings []ExecutionInterruptBinding) error {
	if len(bindings) == 0 || len(bindings) > 128 {
		return executionInvalid("suspension requires 1 to 128 interrupt bindings")
	}
	seen, targets := map[string]bool{}, map[string]bool{}
	for _, b := range bindings {
		if b.IntentID == "" || b.IntentVersion < 1 || b.NativeInterruptID == "" || len(b.NativeInterruptID) > 4096 || seen[b.IntentID] || targets[b.NativeInterruptID] {
			return executionInvalid("invalid or duplicate interrupt binding")
		}
		seen[b.IntentID] = true
		targets[b.NativeInterruptID] = true
		var version int64
		var state, callID string
		err := tx.QueryRowContext(ctx, `SELECT version,state,tool_call_id FROM harness_execution_intents WHERE id=? AND run_id=?`, b.IntentID, runID).Scan(&version, &state, &callID)
		if errors.Is(err, sql.ErrNoRows) {
			return harness.ErrExecutionConflict
		}
		if err != nil {
			return err
		}
		if version != b.IntentVersion || state != "pending" {
			return harness.ErrExecutionConflict
		}
		var receiptState string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=?`, runID, callID).Scan(&receiptState); err != nil {
			return err
		}
		if receiptState != "pending" {
			return harness.ErrExecutionConflict
		}
	}
	var intents, receipts int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_execution_intents WHERE run_id=? AND state='pending'`, runID).Scan(&intents); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE run_id=? AND state='pending'`, runID).Scan(&receipts); err != nil {
		return err
	}
	if intents != len(bindings) || receipts != len(bindings) {
		return fmt.Errorf("%w: checkpoint does not cover every pending tool", harness.ErrExecutionConflict)
	}
	return nil
}

// SuspendExecutionTx atomically promotes the engine's fully joined staged
// checkpoint and waiting state. Earlier native session writes remain separate
// transactions. The caller also ends this ledger attempt in this transaction.
func (s *Store) SuspendExecutionTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, cp ExecutionCheckpoint) (harness.ExecutionState, error) {
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return row.State, err
	}
	if cp.ID != "harness/turn/v1/"+row.State.RunID || len(cp.Data) == 0 || len(cp.Data) > 64*1024*1024 {
		return row.State, executionInvalid("invalid staged execution checkpoint")
	}
	if err = validateExecutionConfig(ctx, tx, row); err != nil {
		return row.State, err
	}
	if err = checkExecutionBindings(ctx, tx, row.State.RunID, cp.Interrupts); err != nil {
		return row.State, err
	}
	m := ExecutionManifest{Version: 1, SessionID: row.State.SessionID, RunID: row.State.RunID, InputID: row.State.InputID, SourceKind: row.SourceKind, SourceSHA: row.SourceSHA, CheckpointID: cp.ID, CheckpointSHA: executionDigest(cp.Data), ConfigSHA: executionConfigDigest(row.Config), InputSHA: row.InputDigest, Interrupts: append([]ExecutionInterruptBinding(nil), cp.Interrupts...)}
	m.NativeHead, err = executionNativeHead(ctx, tx, row.State.SessionID)
	if err != nil {
		return row.State, err
	}
	m.SummaryHead, err = executionSummaryHead(ctx, tx, row.State.SessionID)
	if err != nil {
		return row.State, err
	}
	m.EventCursor, err = executionEventCursor(ctx, tx, row.State.SessionID)
	if err != nil {
		return row.State, err
	}
	m.ReceiptFrontier, err = executionReceiptFrontier(ctx, tx, row.State.SessionID, row.State.RunID)
	if err != nil {
		return row.State, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return row.State, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO eino_checkpoints(id,payload,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at`, cp.ID, cp.Data, now); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_executions SET status='waiting_input',version=version+1,manifest=?,blocked_reason='',updated_at=? WHERE run_id=?`, data, now, row.State.RunID); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='waiting_input',stop_reason='',error='',updated_at=? WHERE id=?`, now, row.State.RunID); err != nil {
		return row.State, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_execution_attempts SET status='waiting_input',finished_at=? WHERE id=?`, now, lease.Scope.AttemptID); err != nil {
		return row.State, err
	}
	if err = revokeExecutionGrants(ctx, tx, lease.Scope.AttemptID); err != nil {
		return row.State, err
	}
	if err = executionAudit(ctx, tx, lease, "waiting_input", map[string]any{"checkpointSHA": m.CheckpointSHA, "eventCursor": m.EventCursor, "nativeHead": m.NativeHead}); err != nil {
		return row.State, err
	}
	row, err = readExecution(ctx, tx, row.State.SessionID, row.State.RunID)
	row.State.Resumable = err == nil
	return row.State, err
}

func validateExecutionManifest(ctx context.Context, tx *sql.Tx, row executionRow) error {
	m := row.Manifest
	if m == nil || m.Version != 1 || m.SessionID != row.State.SessionID || m.RunID != row.State.RunID || m.InputID != row.State.InputID || m.SourceKind != row.SourceKind || m.SourceSHA != row.SourceSHA || m.CheckpointID != "harness/turn/v1/"+row.State.RunID || m.ConfigSHA != executionConfigDigest(row.Config) || m.InputSHA != row.InputDigest {
		return fmt.Errorf("%w: missing or mismatched checkpoint manifest", harness.ErrExecutionUnresumable)
	}
	if err := validateExecutionConfig(ctx, tx, row); err != nil {
		return err
	}
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM eino_checkpoints WHERE id=?`, m.CheckpointID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: checkpoint is missing", harness.ErrExecutionUnresumable)
	}
	if err != nil {
		return err
	}
	if len(data) == 0 || executionDigest(data) != m.CheckpointSHA {
		return fmt.Errorf("%w: checkpoint bytes changed", harness.ErrExecutionUnresumable)
	}
	head, err := executionNativeHead(ctx, tx, row.State.SessionID)
	if err != nil {
		return err
	}
	if head != m.NativeHead {
		return fmt.Errorf("%w: native session advanced", harness.ErrExecutionUnresumable)
	}
	if m.SummaryHead != (ExecutionNativeHead{}) {
		summary, err := executionSummaryHead(ctx, tx, row.State.SessionID)
		if err != nil {
			return err
		}
		if summary != m.SummaryHead {
			return fmt.Errorf("%w: native summary changed", harness.ErrExecutionUnresumable)
		}
	}
	cursor, err := executionEventCursor(ctx, tx, row.State.SessionID)
	if err != nil {
		return err
	}
	if cursor != m.EventCursor {
		return fmt.Errorf("%w: execution events advanced", harness.ErrExecutionUnresumable)
	}
	frontier, err := executionReceiptFrontier(ctx, tx, row.State.SessionID, row.State.RunID)
	if err != nil {
		return err
	}
	if frontier != m.ReceiptFrontier {
		return fmt.Errorf("%w: tool receipts advanced", harness.ErrExecutionUnresumable)
	}
	return checkExecutionBindings(ctx, tx, row.State.RunID, m.Interrupts)
}

// ClaimExecutionResumeTx creates a fresh fenced attempt for the SAME logical
// run/input. The caller must BeginAttemptTx with lease.Scope before committing.
func (s *Store) ClaimExecutionResumeTx(ctx context.Context, tx *sql.Tx, sessionID, owner string, request harness.ResumeExecutionRequest, attemptID string) (ExecutionLease, error) {
	var lease ExecutionLease
	if sessionID == "" || owner == "" || request.RunID == "" || request.ExpectedVersion < 1 || attemptID == "" {
		return lease, executionInvalid("invalid execution resume request")
	}
	row, err := readExecution(ctx, tx, sessionID, request.RunID)
	if err != nil {
		return lease, err
	}
	if row.State.Version != request.ExpectedVersion || row.State.Status != harness.ExecutionWaitingInput {
		return lease, harness.ErrExecutionConflict
	}
	if err = validateExecutionManifest(ctx, tx, row); err != nil {
		return lease, err
	}
	lease = ExecutionLease{OwnerID: owner, InputID: row.State.InputID, Scope: budget.Scope{RootBudgetID: row.RootBudgetID, MemberID: row.State.RunID, SessionID: sessionID, AttemptID: attemptID, Fence: row.State.Attempt + 1}}
	now := timestamp()
	if err = executionCAS(ctx, tx, `UPDATE harness_executions SET status='resuming',version=version+1,attempt_id=?,attempt=attempt+1,blocked_reason='',updated_at=? WHERE run_id=? AND version=? AND status='waiting_input'`, attemptID, now, row.State.RunID, request.ExpectedVersion); err != nil {
		return lease, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_attempts(id,run_id,ordinal,owner_id,status,base_checkpoint_sha,created_at) VALUES(?,?,?,?,'running',?,?)`, attemptID, row.State.RunID, lease.Scope.Fence, owner, row.Manifest.CheckpointSHA, now); err != nil {
		return lease, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='running',stop_reason='',error='',updated_at=? WHERE id=?`, now, row.State.RunID); err != nil {
		return lease, err
	}
	return lease, executionAudit(ctx, tx, lease, "attempt_resuming", map[string]any{"attempt": lease.Scope.Fence, "checkpointSHA": row.Manifest.CheckpointSHA})
}
