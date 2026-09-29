package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// PermissionGrant is bound to an attempt and an immutable intent version. It
// is not valid on another connection, after a disconnect, or for another call.
type PermissionGrant struct {
	ID, IntentID  string
	IntentVersion int64
	Decision      harness.PermissionDecision
}

func executionInteractions(ctx context.Context, tx *sql.Tx, runID string) ([]harness.ExecutionInteraction, error) {
	rows, err := tx.QueryContext(ctx, `SELECT descriptor FROM harness_execution_intents WHERE run_id=? AND state='pending' ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []harness.ExecutionInteraction{}
	for rows.Next() {
		var data []byte
		var x harness.ExecutionInteraction
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &x); err != nil {
			return nil, err
		}
		result = append(result, x)
	}
	return result, rows.Err()
}

// RecordPermissionIntentTx is idempotent only for the exact pending logical
// call. The returned ID is stable across re-interruption; JSON-RPC IDs are not.
func (s *Store) RecordPermissionIntentTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, p harness.PermissionRequest) (harness.ExecutionInteraction, error) {
	var descriptor harness.ExecutionInteraction
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return descriptor, err
	}
	if p.SessionID != row.State.SessionID || p.RunID != row.State.RunID || p.ConfigVersion != row.Config.ConfigVersion || p.ToolCallID == "" || p.ToolName == "" || len(p.Arguments) > 128*1024 || !json.Valid(p.Arguments) {
		return descriptor, executionInvalid("invalid permission intent identity")
	}
	receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
	if err != nil {
		return descriptor, err
	}
	if receipt.State != harness.ReceiptPending || receipt.ToolName != p.ToolName || receipt.ArgumentsDigest != executionDigest(p.Arguments) || receipt.ConfigVersion != p.ConfigVersion {
		return descriptor, harness.ErrReceiptConflict
	}
	var oldRequest, oldDescriptor []byte
	var state string
	err = tx.QueryRowContext(ctx, `SELECT request,descriptor,state FROM harness_execution_intents WHERE run_id=? AND tool_call_id=?`, p.RunID, p.ToolCallID).Scan(&oldRequest, &oldDescriptor, &state)
	if err == nil {
		var old harness.PermissionRequest
		if err = json.Unmarshal(oldRequest, &old); err != nil {
			return descriptor, err
		}
		if state != "pending" || old.SessionID != p.SessionID || old.RunID != p.RunID || old.ConfigVersion != p.ConfigVersion || old.ToolName != p.ToolName || executionDigest(old.Arguments) != executionDigest(p.Arguments) || (p.ID != "" && p.ID != old.ID) {
			return descriptor, harness.ErrExecutionConflict
		}
		err = json.Unmarshal(oldDescriptor, &descriptor)
		return descriptor, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return descriptor, err
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	if len(p.ID) > 256 {
		return descriptor, executionInvalid("permission intent ID is too long")
	}
	descriptor = harness.ExecutionInteraction{ID: p.ID, Kind: "permission", Version: 1, ToolCallID: p.ToolCallID, ToolName: p.ToolName, ArgumentsDigest: receipt.ArgumentsDigest, ArgumentsSummary: receipt.ArgumentsSummary, ConfigVersion: p.ConfigVersion}
	request, err := json.Marshal(p)
	if err != nil {
		return descriptor, err
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		return descriptor, err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_intents(id,run_id,tool_call_id,version,state,request,descriptor,created_at,updated_at) VALUES(?,?,?,1,'pending',?,?,?,?)`, p.ID, p.RunID, p.ToolCallID, request, data, now, now); err != nil {
		return descriptor, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_executions SET status='suspending',version=version+1,updated_at=? WHERE run_id=?`, now, p.RunID); err != nil {
		return descriptor, err
	}
	return descriptor, executionAudit(ctx, tx, lease, "permission_pending", descriptor)
}

// PendingExecutionPermissionsTx returns durable intents only to the active
// trusted attempt. The broker issues NEW reverse requests on the current owner.
func (s *Store) PendingExecutionPermissionsTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease) ([]harness.PermissionRequest, error) {
	if _, err := checkExecutionLease(ctx, tx, lease); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT request FROM harness_execution_intents WHERE run_id=? AND state='pending' ORDER BY id`, lease.Scope.MemberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []harness.PermissionRequest{}
	for rows.Next() {
		var data []byte
		var p harness.PermissionRequest
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func readExecutionIntent(ctx context.Context, tx *sql.Tx, runID, id string) (harness.PermissionRequest, int64, string, error) {
	var p harness.PermissionRequest
	var version int64
	var state string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT request,version,state FROM harness_execution_intents WHERE id=? AND run_id=?`, id, runID).Scan(&data, &version, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return p, version, state, harness.ErrExecutionConflict
	}
	if err != nil {
		return p, version, state, err
	}
	err = json.Unmarshal(data, &p)
	return p, version, state, err
}

// RecordPermissionGrantTx accepts only a response for the current attempt and
// intent version. Disconnect/cancellation is not a synthetic deny response.
func (s *Store) RecordPermissionGrantTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, intentID string, version int64, decision harness.PermissionDecision) (PermissionGrant, error) {
	grant := PermissionGrant{IntentID: intentID, IntentVersion: version, Decision: decision}
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return grant, err
	}
	if row.State.Status != harness.ExecutionResuming {
		return grant, harness.ErrExecutionConflict
	}
	if err = validateExecutionConfig(ctx, tx, row); err != nil {
		return grant, err
	}
	switch decision {
	case harness.AllowOnce, harness.AllowAlways, harness.RejectOnce, harness.RejectAlways:
	default:
		return grant, executionInvalid("disconnect or cancellation cannot authorize an intent")
	}
	p, current, state, err := readExecutionIntent(ctx, tx, row.State.RunID, intentID)
	if err != nil {
		return grant, err
	}
	if state != "pending" || current != version {
		return grant, harness.ErrExecutionConflict
	}
	receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
	if err != nil {
		return grant, err
	}
	if receipt.State != harness.ReceiptPending || receipt.ArgumentsDigest != executionDigest(p.Arguments) || receipt.ConfigVersion != p.ConfigVersion {
		return grant, harness.ErrReceiptConflict
	}
	var existing, existingState string
	err = tx.QueryRowContext(ctx, `SELECT id,decision,state FROM harness_execution_grants WHERE attempt_id=? AND intent_id=?`, lease.Scope.AttemptID, intentID).Scan(&grant.ID, &existing, &existingState)
	if err == nil {
		if existing != string(decision) || existingState != "ready" {
			return grant, harness.ErrExecutionConflict
		}
		return grant, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return grant, err
	}
	grant.ID = NewID()
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_grants(id,run_id,attempt_id,intent_id,intent_version,decision,state,created_at) VALUES(?,?,?,?,?,?,'ready',?)`, grant.ID, row.State.RunID, lease.Scope.AttemptID, intentID, version, decision, timestamp())
	if err != nil {
		return grant, err
	}
	return grant, executionAudit(ctx, tx, lease, "permission_answered", map[string]any{"intentId": intentID, "intentVersion": version, "grantId": grant.ID, "decision": decision})
}

// ValidatePermissionGrantTx only reads the broker's current decision. It does
// not authorize dispatch by itself: the event transaction must consume this
// grant immediately before a tool executes, or when recording a terminal deny.
func (s *Store) ValidatePermissionGrantTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, grant PermissionGrant, p harness.PermissionRequest) (harness.PermissionDecision, error) {
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return "", err
	}
	if err = validateExecutionConfig(ctx, tx, row); err != nil {
		return "", err
	}
	saved, version, state, err := readExecutionIntent(ctx, tx, row.State.RunID, grant.IntentID)
	if err != nil {
		return "", err
	}
	if state != "pending" || version != grant.IntentVersion || saved.SessionID != p.SessionID || saved.RunID != p.RunID || saved.ToolCallID != p.ToolCallID || saved.ToolName != p.ToolName || saved.ConfigVersion != p.ConfigVersion || executionDigest(saved.Arguments) != executionDigest(p.Arguments) {
		return "", harness.ErrExecutionConflict
	}
	receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
	if err != nil {
		return "", err
	}
	if receipt.State != harness.ReceiptPending || receipt.ToolName != p.ToolName || receipt.ConfigVersion != p.ConfigVersion || receipt.ArgumentsDigest != executionDigest(p.Arguments) {
		return "", harness.ErrReceiptConflict
	}
	var decision harness.PermissionDecision
	var grantState string
	err = tx.QueryRowContext(ctx, `SELECT decision,state FROM harness_execution_grants WHERE id=? AND run_id=? AND attempt_id=? AND intent_id=? AND intent_version=?`, grant.ID, row.State.RunID, lease.Scope.AttemptID, grant.IntentID, grant.IntentVersion).Scan(&decision, &grantState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", harness.ErrExecutionConflict
	}
	if err != nil {
		return "", err
	}
	if grantState != "ready" || decision != grant.Decision {
		return "", harness.ErrExecutionConflict
	}
	return decision, nil
}

// ConsumePermissionGrantTx MUST share the tool_execute/terminal-denial SQL
// transaction. It is not a separate preflight check. No non-Tx form is exposed.
// The caller must not invoke any tool unless both that transaction commits and
// the returned decision is allow_once/allow_always.
func (s *Store) ConsumePermissionGrantTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, grant PermissionGrant, p harness.PermissionRequest) (harness.PermissionDecision, error) {
	decision, err := s.ValidatePermissionGrantTx(ctx, tx, lease, grant, p)
	if err != nil {
		return "", err
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_execution_grants SET state='consumed' WHERE id=? AND state='ready'`, grant.ID); err != nil {
		return "", err
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_execution_intents SET state='resolved',version=version+1,updated_at=? WHERE id=? AND version=? AND state='pending'`, timestamp(), grant.IntentID, grant.IntentVersion); err != nil {
		return "", err
	}
	if err = executionAudit(ctx, tx, lease, "permission_consumed", map[string]any{"intentId": grant.IntentID, "grantId": grant.ID, "decision": decision}); err != nil {
		return "", err
	}
	return decision, nil
}

func revokeExecutionGrants(ctx context.Context, tx *sql.Tx, attemptID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE harness_execution_grants SET state='revoked' WHERE attempt_id=? AND state='ready'`, attemptID)
	return err
}

// ExecutionResumeBindingsTx provides broker-owned data to the trusted engine
// adapter after all current targets have answers. It never accepts native IDs
// from the client. The adapter constructs Eino ResumeParams itself.
type ExecutionResumeBinding struct {
	Interrupt ExecutionInterruptBinding
	Grant     PermissionGrant
}

func (s *Store) ExecutionResumeBindingsTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease) ([]ExecutionResumeBinding, error) {
	row, err := checkExecutionLease(ctx, tx, lease)
	if err != nil {
		return nil, err
	}
	if row.Manifest == nil {
		return nil, harness.ErrExecutionUnresumable
	}
	if err = validateExecutionManifest(ctx, tx, row); err != nil {
		return nil, err
	}
	result := make([]ExecutionResumeBinding, 0, len(row.Manifest.Interrupts))
	for _, binding := range row.Manifest.Interrupts {
		grant := PermissionGrant{IntentID: binding.IntentID, IntentVersion: binding.IntentVersion}
		var state string
		err = tx.QueryRowContext(ctx, `SELECT id,decision,state FROM harness_execution_grants WHERE attempt_id=? AND intent_id=? AND intent_version=?`, lease.Scope.AttemptID, binding.IntentID, binding.IntentVersion).Scan(&grant.ID, &grant.Decision, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, harness.ErrExecutionWaitingInput
		}
		if err != nil {
			return nil, err
		}
		if state != "ready" {
			return nil, fmt.Errorf("%w: authorization is no longer current", harness.ErrExecutionConflict)
		}
		result = append(result, ExecutionResumeBinding{Interrupt: binding, Grant: grant})
	}
	return result, nil
}
