package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type backgroundWaitManifest struct {
	Frontier backgroundPermissionManifest
	Waits    []interaction.TaskWaitBinding
}

func (a *BackgroundInteractionAttempt) StageTaskWaits(ctx context.Context, bindings []interaction.TaskWaitBinding) error {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return withExecutionTransaction(persist, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(persist, tx, true); err != nil {
			return err
		}
		var data []byte
		err := tx.QueryRowContext(persist, `SELECT bindings FROM harness_background_wait_stages WHERE task_id=? AND attempt=?`, a.scope.Binding.TaskID, a.scope.Attempt).Scan(&data)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous []interaction.TaskWaitBinding
		if len(data) > 0 {
			if err = json.Unmarshal(data, &previous); err != nil {
				return err
			}
		}
		merged := map[string]interaction.TaskWaitBinding{}
		for _, item := range append(previous, bindings...) {
			if old, exists := merged[item.NativeInterruptID]; exists && old != item {
				return harness.ErrExecutionConflict
			}
			merged[item.NativeInterruptID] = item
		}
		all := make([]interaction.TaskWaitBinding, 0, len(merged))
		for _, item := range merged {
			all = append(all, item)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].NativeInterruptID < all[j].NativeInterruptID })
		if _, err = checkTaskWaits(persist, tx, a.scope.Binding, all); err != nil {
			return err
		}
		data, err = json.Marshal(all)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(persist, `INSERT INTO harness_background_wait_stages(task_id,attempt,bindings) VALUES(?,?,?) ON CONFLICT(task_id,attempt) DO UPDATE SET bindings=excluded.bindings`, a.scope.Binding.TaskID, a.scope.Attempt, data)
		return err
	})
}

func checkTaskWaits(ctx context.Context, tx *sql.Tx, owner background.Binding, waits []interaction.TaskWaitBinding) (bool, error) {
	if len(waits) < 1 || len(waits) > 128 {
		return false, harness.ErrExecutionConflict
	}
	targets, calls := map[string]bool{}, map[string]bool{}
	ready := true
	for _, item := range waits {
		w := item.Wait
		if item.NativeInterruptID == "" || len(item.NativeInterruptID) > 4096 || targets[item.NativeInterruptID] || calls[w.CallID] || w.CallID == "" || w.TaskID == owner.TaskID || w.AfterVersion < 1 || w.SessionID != owner.ChildSessionID || w.RunID != owner.TaskID || w.RootBudgetID != owner.RootBudgetID {
			return false, harness.ErrPermissionDenied
		}
		targets[item.NativeInterruptID], calls[w.CallID] = true, true
		allowed, err := background.OwnsTask(ctx, tx, owner, w.TaskID)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, harness.ErrPermissionDenied
		}
		targetRole, err := background.TaskRoleID(ctx, tx, w.TaskID)
		if err != nil {
			return false, err
		}
		if w.RoleID == targetRole {
			return false, harness.ErrPermissionDenied
		}
		if w.RoleID != "" {
			ownsRole, err := agents.IsDescendant(ctx, tx, owner.RootBudgetID, owner.RoleID(), w.RoleID)
			if err != nil {
				return false, err
			}
			ownsTask, err := agents.IsDescendant(ctx, tx, owner.RootBudgetID, w.RoleID, targetRole)
			if err != nil {
				return false, err
			}
			if !ownsRole || !ownsTask {
				return false, harness.ErrPermissionDenied
			}
		}
		var status, blocked string
		var data []byte
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT t.status,t.version,b.blocked_reason,b.payload FROM eino_background_tasks t JOIN harness_background_bindings b ON b.task_id=t.id WHERE t.id=?`, w.TaskID).Scan(&status, &version, &blocked, &data); err != nil {
			return false, err
		}
		var binding background.Binding
		if err := json.Unmarshal(data, &binding); err != nil {
			return false, harness.ErrTaskOriginConflict
		}
		if binding.TaskID != w.TaskID || binding.RootBudgetID != owner.RootBudgetID || binding.RootSession() != owner.RootSession() || version < w.AfterVersion {
			return false, harness.ErrPermissionDenied
		}
		var uncertain bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('started','uncertain'))`, binding.ChildSessionID).Scan(&uncertain); err != nil {
			return false, err
		}
		ready = ready && terminalBackgroundStatus(status) && blocked == "" && !uncertain
	}
	return ready, nil
}

func (s *BackgroundInteractionStore) readTaskWait(ctx context.Context, tx *sql.Tx, id string) (backgroundWaitManifest, string, error) {
	var m backgroundWaitManifest
	var state string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT state,payload FROM harness_background_wait_manifests WHERE task_id=?`, id).Scan(&state, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return m, state, harness.ErrNotFound
	}
	if err != nil {
		return m, state, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, state, err
	}
	if m.Frontier.Version != 1 || m.Frontier.Binding.TaskID != id || len(m.Frontier.Interrupts) != 0 || m.Frontier.Attempt < 1 || m.Frontier.TaskVersion < 1 {
		return m, state, harness.ErrTaskOriginConflict
	}
	return m, state, nil
}

// Mixed permissions and waits use the permission manifest. Approving that
// batch resumes only permission targets; unfinished wait tools interrupt again.
// A pure wait publishes its own frontier and can be resumed automatically.
func (s *BackgroundInteractionStore) captureTaskWait(ctx context.Context, tx *sql.Tx, scope background.TaskScope, after *bt.Task) (bool, error) {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT bindings FROM harness_background_wait_stages WHERE task_id=? AND attempt=?`, scope.Binding.TaskID, scope.Attempt).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var waits []interaction.TaskWaitBinding
	if err = json.Unmarshal(data, &waits); err != nil {
		return true, err
	}
	if _, err = checkTaskWaits(ctx, tx, scope.Binding, waits); err != nil {
		return true, err
	}
	var permissions int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_background_permission_intents WHERE task_id=? AND state='pending'`, scope.Binding.TaskID).Scan(&permissions); err != nil {
		return true, err
	}
	if permissions > 0 {
		return false, nil
	}
	frontier, err := s.captureFrontier(ctx, tx, scope, after)
	if err != nil {
		return true, err
	}
	data, err = json.Marshal(backgroundWaitManifest{Frontier: frontier, Waits: waits})
	if err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_wait_manifests(task_id,state,payload) VALUES(?,'waiting',?) ON CONFLICT(task_id) DO UPDATE SET state='waiting',payload=excluded.payload`, scope.Binding.TaskID, data); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_drain_manifests SET state='closed' WHERE task_id=?`, scope.Binding.TaskID); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_background_permission_grants SET state='revoked' WHERE task_id=? AND state='ready'`, scope.Binding.TaskID); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_background_interrupt_stages WHERE task_id=?`, scope.Binding.TaskID); err != nil {
		return true, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM harness_background_wait_stages WHERE task_id=?`, scope.Binding.TaskID)
	return true, err
}

func taskWaitResumeData(m backgroundWaitManifest) (json.RawMessage, error) {
	targets := map[string]any{}
	for _, item := range m.Waits {
		targets[item.NativeInterruptID] = map[string]string{"wait": m.Frontier.ID}
	}
	return json.Marshal(targets)
}

func (s *BackgroundInteractionStore) ReadyWakeups(ctx context.Context) ([]background.Wakeup, error) {
	var ready []background.Wakeup
	var failures []error
	after := ""
	for len(ready) < 100 {
		rows, err := s.store.db.QueryContext(ctx, `SELECT m.task_id FROM harness_background_wait_manifests m JOIN eino_background_tasks t ON t.id=m.task_id WHERE m.state='waiting' AND t.status='waiting_input' AND m.task_id>? AND NOT EXISTS(SELECT 1 FROM harness_background_stops stop WHERE stop.task_id=m.task_id) ORDER BY m.task_id LIMIT 100`, after)
		if err != nil {
			return ready, errors.Join(append(failures, err)...)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return ready, errors.Join(append(failures, err)...)
		}
		for _, id := range ids {
			after = id
			var wakeup *background.Wakeup
			err = withExecutionTransaction(ctx, s.store, func(tx *sql.Tx) error {
				m, state, err := s.readTaskWait(ctx, tx, id)
				if err != nil {
					return err
				}
				if state != "waiting" {
					return nil
				}
				task, err := readBackgroundNativeTask(ctx, tx, id)
				if err != nil {
					return err
				}
				if task.Status != bt.StatusWaitingInput || task.Version != m.Frontier.TaskVersion {
					return nil
				}
				done, err := checkTaskWaits(ctx, tx, m.Frontier.Binding, m.Waits)
				if err != nil || !done {
					return err
				}
				if err = s.validateFrontier(ctx, tx, m.Frontier, task); err != nil {
					return err
				}
				data, err := taskWaitResumeData(m)
				if err == nil {
					wakeup = &background.Wakeup{TaskID: id, Version: task.Version, Data: data}
				}
				return err
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("background wait %s: %w", id, err))
				if ctx.Err() != nil {
					return ready, errors.Join(failures...)
				}
				continue
			}
			if wakeup != nil {
				ready = append(ready, *wakeup)
			}
			if len(ready) == 100 {
				return ready, errors.Join(failures...)
			}
		}
		if len(ids) < 100 {
			break
		}
	}
	return ready, errors.Join(failures...)
}

func (s *BackgroundInteractionStore) CommitWakeupTx(ctx context.Context, tx *sql.Tx, binding background.Binding, wakeup background.Wakeup) error {
	m, state, err := s.readTaskWait(ctx, tx, binding.TaskID)
	if err != nil {
		return err
	}
	if state != "waiting" || m.Frontier.Binding != binding || wakeup.TaskID != binding.TaskID || wakeup.Version != m.Frontier.TaskVersion {
		return harness.ErrExecutionConflict
	}
	task, err := readBackgroundNativeTask(ctx, tx, binding.TaskID)
	if err != nil {
		return err
	}
	if task.Status != bt.StatusWaitingInput || task.Version != wakeup.Version || task.Attempt != m.Frontier.Attempt {
		return harness.ErrExecutionConflict
	}
	if err = s.validateFrontier(ctx, tx, m.Frontier, task); err != nil {
		return err
	}
	done, err := checkTaskWaits(ctx, tx, binding, m.Waits)
	if err != nil {
		return err
	}
	if !done {
		return harness.ErrBusy
	}
	expected, err := taskWaitResumeData(m)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, wakeup.Data) {
		return harness.ErrPermissionDenied
	}
	return executionCAS(ctx, tx, `UPDATE harness_background_wait_manifests SET state='released' WHERE task_id=? AND state='waiting'`, binding.TaskID)
}

func (s *BackgroundInteractionStore) validateReleasedWait(ctx context.Context, tx *sql.Tx, binding background.Binding, task *bt.Task, attempt int64, starting bool) (bool, error) {
	m, state, err := s.readTaskWait(ctx, tx, binding.TaskID)
	if errors.Is(err, harness.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if state == "closed" {
		return false, nil
	}
	if state != "released" || m.Frontier.Binding != binding || m.Frontier.Attempt+1 != attempt || starting && task.Version != m.Frontier.TaskVersion+1 {
		return true, harness.ErrExecutionConflict
	}
	if err = s.validateFrontier(ctx, tx, m.Frontier, task); err != nil {
		return true, err
	}
	expected, err := taskWaitResumeData(m)
	if err != nil {
		return true, err
	}
	if !bytes.Equal(expected, task.PendingResume) {
		return true, harness.ErrPermissionDenied
	}
	return true, nil
}

func (s *BackgroundInteractionStore) taskWaitInteraction(ctx context.Context, tx *sql.Tx, binding background.Binding, task *bt.Task) (*harness.BackgroundInteraction, bool, error) {
	m, state, err := s.readTaskWait(ctx, tx, binding.TaskID)
	if errors.Is(err, harness.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if state == "closed" {
		return nil, false, nil
	}
	if state != "waiting" || m.Frontier.TaskVersion != task.Version {
		return nil, true, harness.ErrExecutionConflict
	}
	result := &harness.BackgroundInteraction{ID: m.Frontier.ID, TaskVersion: task.Version, Kind: "task_wait", WaitingInputs: []harness.ExecutionInteraction{}}
	for _, item := range m.Waits {
		result.WaitingTasks = append(result.WaitingTasks, item.Wait.TaskID)
	}
	if err = s.validateFrontier(ctx, tx, m.Frontier, task); err != nil {
		result.BlockedReason = err.Error()
	}
	return result, true, nil
}
