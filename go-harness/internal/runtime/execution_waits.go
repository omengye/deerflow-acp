package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	fastjson "github.com/segmentio/encoding/json"
)

func checkExecutionInterruptSet(permissions []ExecutionInterruptBinding, waits []interaction.TaskWaitBinding) error {
	if n := len(permissions) + len(waits); n < 1 || n > 128 {
		return fmt.Errorf("%w: suspension requires 1 to 128 interrupt targets", harness.ErrExecutionConflict)
	}
	seen := map[string]bool{}
	for _, permission := range permissions {
		seen[permission.NativeInterruptID] = true
	}
	for _, wait := range waits {
		if seen[wait.NativeInterruptID] {
			return fmt.Errorf("%w: duplicate native interrupt target", harness.ErrExecutionConflict)
		}
		seen[wait.NativeInterruptID] = true
	}
	return nil
}

// Wait metadata is also bound inside the hashed adapter envelope. Changing a
// manifest's native address or role must not retarget an opaque checkpoint.
// Legacy permission-only fixtures/checkpoints need no new envelope fields.
func checkExecutionWaitBytes(data []byte, waits []interaction.TaskWaitBinding) error {
	var envelope struct {
		TaskWaitsEnabled bool                          `json:"taskWaitsEnabled"`
		TaskWaits        []interaction.TaskWaitBinding `json:"taskWaits"`
	}
	// Native nested-role checkpoints can approach the 64 MiB storage limit.
	// Scan their opaque JSON strings in blocks rather than encoding/json's
	// race-instrumented byte-at-a-time state machine while holding SQLite.
	// Keep full JSON validation and the same field decoding/binding checks.
	decodeErr := fastjson.Unmarshal(data, &envelope)
	if decodeErr != nil {
		// The standard decoder validates syntax before populating fields and
		// continues after type errors. Preserve that legacy behavior on errors.
		envelope.TaskWaitsEnabled, envelope.TaskWaits = false, nil
		decodeErr = json.Unmarshal(data, &envelope)
	}
	if len(waits) == 0 && len(envelope.TaskWaits) == 0 {
		return nil
	}
	if decodeErr != nil || !envelope.TaskWaitsEnabled || !slices.Equal(envelope.TaskWaits, waits) {
		return fmt.Errorf("%w: task wait bindings differ from checkpoint", harness.ErrExecutionUnresumable)
	}
	return nil
}

// Dependency versions may advance during the wait. Their scope cannot change,
// and only a terminal, unblocked dependency can make a pure wait resumable.
// This runs in the same transaction as publication/claim, never via an
// asynchronous public task projection or a caller-supplied task status.
func checkExecutionTaskWaits(ctx context.Context, tx *sql.Tx, row executionRow, waits []interaction.TaskWaitBinding) (bool, error) {
	if len(waits) > 128 {
		return false, harness.ErrExecutionConflict
	}
	ready := true
	targets, calls := map[string]bool{}, map[string]bool{}
	for _, item := range waits {
		w := item.Wait
		if item.NativeInterruptID == "" || len(item.NativeInterruptID) > 4096 || targets[item.NativeInterruptID] || calls[w.CallID] || w.CallID == "" || w.TaskID == "" || w.AfterVersion < 1 || w.SessionID != row.State.SessionID || w.RunID != row.State.RunID || w.RootBudgetID != row.RootBudgetID {
			return false, harness.ErrPermissionDenied
		}
		targets[item.NativeInterruptID], calls[w.CallID] = true, true
		var data []byte
		var status, blocked, treeRoot, parentTask string
		var version int64
		err := tx.QueryRowContext(ctx, `SELECT b.payload,b.blocked_reason,t.status,t.version,r.root_session_id,r.parent_task_id FROM harness_background_bindings b JOIN eino_background_tasks t ON t.id=b.task_id JOIN harness_background_tree r ON r.task_id=b.task_id WHERE b.task_id=?`, w.TaskID).Scan(&data, &blocked, &status, &version, &treeRoot, &parentTask)
		if errors.Is(err, sql.ErrNoRows) {
			return false, harness.ErrNotFound
		}
		if err != nil {
			return false, err
		}
		var binding background.Binding
		if err = json.Unmarshal(data, &binding); err != nil {
			return false, fmt.Errorf("%w: invalid waited task binding", harness.ErrExecutionUnresumable)
		}
		if binding.TaskID != w.TaskID || binding.RootBudgetID != row.RootBudgetID || binding.RootSession() != row.State.SessionID || binding.RootSession() != treeRoot || binding.ParentTaskID != parentTask || version < w.AfterVersion {
			return false, harness.ErrPermissionDenied
		}
		if w.RoleID != "" {
			allowed, err := agents.IsDescendant(ctx, tx, row.RootBudgetID, w.RoleID, binding.RoleID())
			if err != nil {
				return false, err
			}
			if !allowed || w.RoleID == binding.RoleID() {
				return false, harness.ErrPermissionDenied
			}
		}
		var uncertain bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('started','uncertain'))`, binding.ChildSessionID).Scan(&uncertain); err != nil {
			return false, err
		}
		ready = ready && terminalBackgroundStatus(status) && blocked == "" && !uncertain
	}
	return ready, nil
}

func executionResumeReadiness(ctx context.Context, tx *sql.Tx, row executionRow) error {
	if row.Manifest == nil {
		return harness.ErrExecutionUnresumable
	}
	// Permissions are independent of descendant completion. Resume that batch
	// first; still-pending waits will suspend again without model/tool replay.
	if len(row.Manifest.Interrupts) > 0 {
		return nil
	}
	ready, err := checkExecutionTaskWaits(ctx, tx, row, row.Manifest.TaskWaits)
	if err == nil && !ready {
		err = harness.ErrExecutionWaitingInput
	}
	return err
}

func (a *executionAttempt) TaskWaitReady(ctx context.Context, wait interaction.TaskWait) (bool, error) {
	if err := a.service.Coordinator.Authorize(a.lease.Scope.SessionID, a.lease.OwnerID); err != nil {
		return false, err
	}
	var ready bool
	err := withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, a.lease)
		if err != nil {
			return err
		}
		ready, err = checkExecutionTaskWaits(ctx, tx, row, []interaction.TaskWaitBinding{{Wait: wait, NativeInterruptID: "preflight"}})
		return err
	})
	return ready, err
}

// Every restored wait comes from the committed manifest; the transport never
// accepts this map. Recheck it under the new execution lease before native I/O.
func executionWaitTargets(ctx context.Context, tx *sql.Tx, row executionRow) (map[string]interaction.TaskWait, error) {
	result := map[string]interaction.TaskWait{}
	if row.Manifest == nil {
		return result, nil
	}
	if err := validateExecutionManifest(ctx, tx, row); err != nil {
		return nil, err
	}
	for _, item := range row.Manifest.TaskWaits {
		ready, err := checkExecutionTaskWaits(ctx, tx, row, []interaction.TaskWaitBinding{item})
		if err != nil {
			return nil, err
		}
		if ready {
			result[item.NativeInterruptID] = item.Wait
		}
	}
	if len(row.Manifest.Interrupts) == 0 && len(result) != len(row.Manifest.TaskWaits) {
		return nil, harness.ErrExecutionWaitingInput
	}
	return result, nil
}
