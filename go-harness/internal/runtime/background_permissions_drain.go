package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

func (s *BackgroundInteractionStore) captureFrontier(ctx context.Context, tx *sql.Tx, scope background.TaskScope, task *bt.Task) (backgroundPermissionManifest, error) {
	m := backgroundPermissionManifest{Version: 1, ID: NewID(), Binding: scope.Binding, Attempt: scope.Attempt, TaskVersion: task.Version}
	req, err := s.bindingTx(ctx, tx, scope.Binding, true)
	if err != nil {
		return m, err
	}
	if err = backgroundReceiptFrontier(ctx, tx, scope, false); err != nil {
		return m, err
	}
	var checkpoint []byte
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM eino_checkpoints WHERE id=?`, scope.Binding.TaskID+"/checkpoint").Scan(&checkpoint); err != nil {
		return m, err
	}
	if len(checkpoint) == 0 || len(checkpoint) > 64*1024*1024 || len(task.Checkpoint) == 0 {
		return m, harness.ErrExecutionUnresumable
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		return m, err
	}
	m.CheckpointSHA, m.NativeTaskSHA = executionDigest(checkpoint), executionDigest(task.Checkpoint)
	m.InputSHA, m.ConfigSHA = executionDigest(input), executionConfigDigest(req.Session)
	if m.NativeHead, err = executionNativeHead(ctx, tx, scope.Binding.ChildSessionID); err != nil {
		return m, err
	}
	if m.EventCursor, err = executionEventCursor(ctx, tx, scope.Binding.ChildSessionID); err != nil {
		return m, err
	}
	m.ReceiptFrontier, err = executionReceiptFrontier(ctx, tx, scope.Binding.ChildSessionID, scope.Binding.TaskID)
	return m, err
}

func (s *BackgroundInteractionStore) readDrain(ctx context.Context, tx *sql.Tx, binding background.Binding) (backgroundPermissionManifest, string, error) {
	var m backgroundPermissionManifest
	var data []byte
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state,payload FROM harness_background_drain_manifests WHERE task_id=?`, binding.TaskID).Scan(&state, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return m, state, harness.ErrNotFound
	}
	if err != nil {
		return m, state, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, state, err
	}
	if m.Version != 1 || m.Binding != binding || m.Attempt < 1 || m.TaskVersion < 1 || len(m.Interrupts) != 0 {
		return m, state, harness.ErrTaskOriginConflict
	}
	return m, state, nil
}

// A drain checkpoint is a new native execution frontier, independent of the
// prior permission batch. Outstanding grants never cross the drained attempt.
func (s *BackgroundInteractionStore) suspendTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	if scope.Attempt != before.Attempt || after.Attempt != before.Attempt || after.Version != before.Version+1 || len(after.PendingResume) != 0 {
		return harness.ErrExecutionConflict
	}
	m, err := s.captureFrontier(ctx, tx, scope, after)
	if err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_drain_manifests(task_id,state,payload) VALUES(?,'suspended',?) ON CONFLICT(task_id) DO UPDATE SET state='suspended',payload=excluded.payload`, scope.Binding.TaskID, data); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_grants SET state='revoked' WHERE task_id=? AND state='ready'`, scope.Binding.TaskID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM harness_background_interrupt_stages WHERE task_id=?`, scope.Binding.TaskID)
	return err
}

func (s *BackgroundInteractionStore) releaseDrainTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	m, state, err := s.readDrain(ctx, tx, scope.Binding)
	if err != nil {
		return err
	}
	if state != "suspended" || m.TaskVersion != before.Version || m.Attempt != before.Attempt || after.Attempt != before.Attempt || after.Version != before.Version+1 || len(after.PendingResume) != 0 {
		return harness.ErrExecutionConflict
	}
	if err = s.validateFrontier(ctx, tx, m, before); err != nil {
		return err
	}
	return executionCAS(ctx, tx, `UPDATE harness_background_drain_manifests SET state='released' WHERE task_id=? AND state='suspended'`, scope.Binding.TaskID)
}

func (s *BackgroundInteractionStore) validateReleasedDrain(ctx context.Context, tx *sql.Tx, binding background.Binding, task *bt.Task, attempt int64, starting bool) (bool, error) {
	m, state, err := s.readDrain(ctx, tx, binding)
	if errors.Is(err, harness.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if state == "closed" {
		return false, nil
	}
	if state != "released" || m.Attempt+1 != attempt || len(task.PendingResume) != 0 || starting && task.Version != m.TaskVersion+1 {
		return true, harness.ErrExecutionConflict
	}
	return true, s.validateFrontier(ctx, tx, m, task)
}
