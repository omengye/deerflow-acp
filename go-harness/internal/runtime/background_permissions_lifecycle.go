package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

// TransitionTx runs in the native task transaction after checkpoint promotion
// and business/budget projection, but before its child lease is released. The
// native host has already validated that cleanup joined. It must not call the
// active engine fence here: native transition code holds its attempt mutex.
func (s *BackgroundInteractionStore) TransitionTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	if tx == nil || before == nil || after == nil || before.Spec.ID != scope.Binding.TaskID || after.Spec.ID != before.Spec.ID || before.Spec.SessionID != scope.Binding.ParentSessionID || after.Spec.SessionID != before.Spec.SessionID {
		return harness.ErrTaskOriginConflict
	}
	if _, err := s.bindingTx(ctx, tx, scope.Binding, false); err != nil {
		return err
	}
	if before.Status == bt.StatusPending && after.Status == bt.StatusRunning {
		if drained, err := s.validateReleasedDrain(ctx, tx, scope.Binding, before, after.Attempt, true); drained || err != nil {
			return err
		}
		m, state, err := s.readManifest(ctx, tx, scope.Binding)
		if errors.Is(err, harness.ErrNotFound) {
			if after.Attempt != 1 || len(before.Checkpoint) != 0 || len(before.PendingResume) != 0 {
				return harness.ErrExecutionUnresumable
			}
			return nil
		}
		if err != nil {
			return err
		}
		if state != "approved" || after.Attempt != m.Attempt+1 || before.Version != m.TaskVersion+1 {
			return harness.ErrExecutionConflict
		}
		if err = s.validateManifest(ctx, tx, m, before); err != nil {
			return err
		}
		targets, err := s.grantTargetsTx(ctx, tx, m, after.Attempt)
		if err != nil {
			return err
		}
		return verifyBackgroundResumeData(before.PendingResume, targets)
	}
	if before.Status == bt.StatusRunning && after.Status == bt.StatusSuspended {
		return s.suspendTx(ctx, tx, scope, before, after)
	}
	if before.Status == bt.StatusSuspended && after.Status == bt.StatusPending {
		return s.releaseDrainTx(ctx, tx, scope, before, after)
	}
	if before.Status == bt.StatusRunning && after.Status == bt.StatusWaitingInput {
		if scope.Attempt != before.Attempt || after.Attempt != before.Attempt || after.Version != before.Version+1 {
			return harness.ErrExecutionConflict
		}
		var staged []byte
		if err := tx.QueryRowContext(ctx, `SELECT bindings FROM harness_background_interrupt_stages WHERE task_id=? AND attempt=?`, scope.Binding.TaskID, scope.Attempt).Scan(&staged); errors.Is(err, sql.ErrNoRows) {
			return harness.ErrExecutionUnresumable
		} else if err != nil {
			return err
		}
		var bindings []interaction.ExecutionInterruptBinding
		if err := json.Unmarshal(staged, &bindings); err != nil {
			return err
		}
		if err := checkBackgroundBindings(ctx, tx, scope.Binding, bindings); err != nil {
			return err
		}
		req, err := s.bindingTx(ctx, tx, scope.Binding, true)
		if err != nil {
			return err
		}
		var checkpoint []byte
		if err = tx.QueryRowContext(ctx, `SELECT payload FROM eino_checkpoints WHERE id=?`, scope.Binding.TaskID+"/checkpoint").Scan(&checkpoint); err != nil {
			return err
		}
		if len(checkpoint) == 0 || len(checkpoint) > 64*1024*1024 || len(after.Checkpoint) == 0 {
			return harness.ErrExecutionUnresumable
		}
		input, _ := json.Marshal(req.Input)
		m := backgroundPermissionManifest{Version: 1, ID: NewID(), Binding: scope.Binding, Attempt: scope.Attempt, TaskVersion: after.Version, CheckpointSHA: executionDigest(checkpoint), NativeTaskSHA: executionDigest(after.Checkpoint), InputSHA: executionDigest(input), ConfigSHA: executionConfigDigest(req.Session), Interrupts: bindings}
		if m.NativeHead, err = executionNativeHead(ctx, tx, scope.Binding.ChildSessionID); err != nil {
			return err
		}
		if m.SummaryHead, err = executionSummaryHead(ctx, tx, scope.Binding.ChildSessionID); err != nil {
			return err
		}
		if m.EventCursor, err = executionEventCursor(ctx, tx, scope.Binding.ChildSessionID); err != nil {
			return err
		}
		if m.ReceiptFrontier, err = executionReceiptFrontier(ctx, tx, scope.Binding.ChildSessionID, scope.Binding.TaskID); err != nil {
			return err
		}
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_permission_manifests(task_id,id,state,payload) VALUES(?,?,'waiting',?) ON CONFLICT(task_id) DO UPDATE SET id=excluded.id,state='waiting',payload=excluded.payload`, scope.Binding.TaskID, m.ID, data); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_grants SET state='revoked' WHERE task_id=? AND state='ready'`, scope.Binding.TaskID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE harness_background_drain_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM harness_background_interrupt_stages WHERE task_id=?`, scope.Binding.TaskID)
		return err
	}
	if after.Status == bt.StatusCompleted || after.Status == bt.StatusFailed || after.Status == bt.StatusCanceled {
		if _, err := tx.ExecContext(ctx, `UPDATE harness_background_drain_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_background_permission_grants SET state='revoked' WHERE task_id=? AND state='ready'`, scope.Binding.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_background_permission_intents SET state='cancelled',version=version+1 WHERE task_id=? AND state='pending'`, scope.Binding.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_background_permission_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM harness_background_interrupt_stages WHERE task_id=?`, scope.Binding.TaskID)
		return err
	}
	return nil
}

func (s *BackgroundInteractionStore) grantTargetsTx(ctx context.Context, tx *sql.Tx, m backgroundPermissionManifest, attempt int64) (map[string]interaction.PermissionResume, error) {
	result := make(map[string]interaction.PermissionResume, len(m.Interrupts))
	for _, b := range m.Interrupts {
		var id, state string
		err := tx.QueryRowContext(ctx, `SELECT id,state FROM harness_background_permission_grants WHERE task_id=? AND attempt=? AND intent_id=? AND intent_version=? AND task_version=?`, m.Binding.TaskID, attempt, b.IntentID, b.IntentVersion, m.TaskVersion).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, harness.ErrPermissionDenied
		}
		if err != nil {
			return nil, err
		}
		if state != "ready" {
			return nil, harness.ErrExecutionConflict
		}
		result[b.NativeInterruptID] = interaction.PermissionResume{IntentID: b.IntentID, IntentVersion: b.IntentVersion, GrantID: id}
	}
	return result, nil
}

func verifyBackgroundResumeData(data []byte, targets map[string]interaction.PermissionResume) error {
	var actual map[string]interaction.PermissionResume
	if err := decodeStrictBackgroundJSON(data, &actual); err != nil {
		return err
	}
	if len(actual) != len(targets) || len(actual) == 0 {
		return harness.ErrExecutionConflict
	}
	for id, target := range targets {
		if actual[id] != target {
			return harness.ErrExecutionConflict
		}
	}
	return nil
}
