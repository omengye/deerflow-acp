package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

// Observation only stages evidence. The native task transaction commits it
// with a completed result or a validated checkpoint after resources join.
func (a *BackgroundInteractionAttempt) ObserveTaskResult(ctx context.Context, d interaction.TaskResultDelivery) error {
	return withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		if err := checkBackgroundDeliveryTx(ctx, tx, a.scope.Binding, d); err != nil {
			return err
		}
		data, err := json.Marshal(d)
		if err != nil {
			return err
		}
		var prior []byte
		err = tx.QueryRowContext(ctx, `SELECT payload FROM harness_background_delivery_stages WHERE task_id=? AND attempt=? AND call_id=?`, a.scope.Binding.TaskID, a.scope.Attempt, d.Wait.CallID).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, data) {
				return harness.ErrExecutionConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_delivery_stages(task_id,attempt,call_id,payload) VALUES(?,?,?,?)`, a.scope.Binding.TaskID, a.scope.Attempt, d.Wait.CallID, data)
		return err
	})
}

func checkBackgroundDeliveryTx(ctx context.Context, tx *sql.Tx, owner background.Binding, d interaction.TaskResultDelivery) error {
	ready, err := checkTaskWaits(ctx, tx, owner, []interaction.TaskWaitBinding{{Wait: d.Wait, NativeInterruptID: "delivery"}})
	if err != nil {
		return err
	}
	if !ready {
		return harness.ErrExecutionWaitingInput
	}
	task, err := readBackgroundNativeTask(ctx, tx, d.Wait.TaskID)
	if err != nil {
		return err
	}
	if task.Version != d.TaskVersion || d.TaskVersion < d.Wait.AfterVersion {
		return harness.ErrExecutionConflict
	}
	return nil
}

func (s *BackgroundInteractionStore) transitionTaskDeliveriesTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	if before.Status == bt.StatusRunning && (after.Status == bt.StatusCompleted || after.Status == bt.StatusWaitingInput || after.Status == bt.StatusSuspended) {
		if scope.Attempt != before.Attempt || after.Attempt != before.Attempt || after.Version != before.Version+1 {
			return harness.ErrExecutionConflict
		}
		rows, err := tx.QueryContext(ctx, `SELECT call_id,payload FROM harness_background_delivery_stages WHERE task_id=? AND attempt=? ORDER BY call_id`, scope.Binding.TaskID, scope.Attempt)
		if err != nil {
			return err
		}
		var deliveries []interaction.TaskResultDelivery
		for rows.Next() {
			var call string
			var data []byte
			var d interaction.TaskResultDelivery
			if err = rows.Scan(&call, &data); err != nil {
				break
			}
			if err = json.Unmarshal(data, &d); err != nil {
				break
			}
			if d.Wait.CallID != call {
				err = harness.ErrExecutionConflict
				break
			}
			deliveries = append(deliveries, d)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			if err = checkBackgroundDeliveryTx(ctx, tx, scope.Binding, d); err != nil {
				return err
			}
			w := d.Wait
			if w.RoleID == "" {
				// Version 4 task waits did not carry the role ID. Their only
				// allowed worker recipient was the background root invocation.
				w.RoleID = scope.Binding.RoleID()
			}
			if err = background.RecordTaskDeliveryTx(ctx, tx, background.TaskDelivery{TaskID: w.TaskID, Version: d.TaskVersion, RecipientRole: w.RoleID, RunID: w.RunID, RootBudgetID: w.RootBudgetID, SessionID: w.SessionID}); err != nil {
				return err
			}
		}
	}
	if before.Status == bt.StatusRunning && after.Status != bt.StatusRunning || terminalBackgroundStatus(string(after.Status)) {
		_, err := tx.ExecContext(ctx, `DELETE FROM harness_background_delivery_stages WHERE task_id=?`, scope.Binding.TaskID)
		return err
	}
	return nil
}

var _ interaction.TaskResultObserver = (*BackgroundInteractionAttempt)(nil)
