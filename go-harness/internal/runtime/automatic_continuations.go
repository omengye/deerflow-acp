package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type automaticNotificationKey struct{}

func migrateAutomaticContinuations(ctx context.Context, db *sql.DB) error {
	if err := background.MigrateNotificationWork(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_automatic_continuations (
 root_id TEXT PRIMARY KEY,session_id TEXT NOT NULL,policy_digest TEXT NOT NULL,
 max_runs INTEGER NOT NULL,used INTEGER NOT NULL,next_at INTEGER NOT NULL);`)
	return err
}

func (a *executionAttempt) ObserveTaskResult(ctx context.Context, d interaction.TaskResultDelivery) error {
	if err := a.service.Coordinator.Authorize(a.lease.Scope.SessionID, a.lease.OwnerID); err != nil {
		return err
	}
	err := withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		row, err := checkExecutionLease(ctx, tx, a.lease)
		if err != nil {
			return err
		}
		ready, err := checkExecutionTaskWaits(ctx, tx, row, []interaction.TaskWaitBinding{{Wait: d.Wait, NativeInterruptID: "delivery"}})
		if err != nil {
			return err
		}
		if !ready {
			return harness.ErrExecutionWaitingInput
		}
		var version int64
		if err = tx.QueryRowContext(ctx, `SELECT version FROM eino_background_tasks WHERE id=?`, d.Wait.TaskID).Scan(&version); err != nil {
			return err
		}
		if version != d.TaskVersion {
			return harness.ErrExecutionConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.taskResults = append(a.taskResults, d)
	return nil
}

func commitTaskDeliveriesTx(ctx context.Context, tx *sql.Tx, lease ExecutionLease, deliveries []interaction.TaskResultDelivery) error {
	for _, d := range deliveries {
		w := d.Wait
		if w.RunID != lease.Scope.MemberID || w.SessionID != lease.Scope.SessionID || w.RootBudgetID != lease.Scope.RootBudgetID {
			return harness.ErrExecutionConflict
		}
		if err := background.RecordTaskDeliveryTx(ctx, tx, background.TaskDelivery{TaskID: w.TaskID, Version: d.TaskVersion, RecipientRole: w.RoleID, RunID: w.RunID, RootBudgetID: w.RootBudgetID, SessionID: w.SessionID}); err != nil {
			return err
		}
	}
	return nil
}

// These are stable public scheduling states. A private role's result is never
// retargeted to the root merely because both share the foreground session.
func notificationEligibilityTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, n bt.Notification) (state, detail string, err error) {
	if !terminalBackgroundStatus(string(n.Kind)) && n.Kind != roleResultNotification {
		return "ignored", "Only terminal notifications create continuation runs.", nil
	}
	if spec.Automatic == nil || !spec.Automatic.Enabled {
		return "disabled", "Automatic continuation was not enabled when this task was created.", nil
	}
	var stopped, delivered bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_automatic_stops WHERE root_id=?),EXISTS(SELECT 1 FROM harness_task_deliveries WHERE task_id=? AND task_version>=? AND recipient_role=?)`, b.RootBudgetID, b.TaskID, n.Version, b.ParentInvocationID).Scan(&stopped, &delivered)
	if err != nil {
		return
	}
	if stopped {
		return "stopped", "Automatic work for this root was explicitly stopped.", nil
	}
	if delivered && n.Kind != roleResultNotification {
		return "consumed", "The saved parent execution already received this task result.", nil
	}
	if b.ParentInvocationID != "" || b.ParentTaskID != "" || b.ParentSessionID != b.RootSession() || n.Kind == roleResultNotification {
		if b.ParentTaskID != "" || b.ParentSessionID != b.RootSession() {
			if _, roleErr := backgroundContinuationTargetTx(ctx, tx, b, spec); roleErr != nil {
				if errors.Is(roleErr, errBackgroundParentStopped) {
					return "stopped", "The original background parent was stopped.", nil
				}
				if errors.Is(roleErr, errRoleConversationUnavailable) {
					return "waiting_parent", "The original background parent has not committed an eligible terminal conversation.", nil
				}
				if blockedContinuationSource(roleErr) {
					return "blocked", "The original background parent state or tool outcome requires review.", nil
				}
				return "", "", roleErr
			}
		} else if _, _, roleErr := notificationParentRoleTx(ctx, tx, b, spec, n); roleErr != nil {
			if errors.Is(roleErr, errRoleConversationUnavailable) {
				return "waiting_parent", "The saved parent role has no eligible committed conversation.", nil
			}
			if blockedContinuationSource(roleErr) {
				return "blocked", "The saved parent conversation or result source requires review.", nil
			}
			return "", "", roleErr
		}
	}
	if spec.Automatic.Limit == 0 {
		return "limit_reached", "The root automatic continuation limit is zero.", nil
	}
	var limit, used int
	var digest string
	var next int64
	err = tx.QueryRowContext(ctx, `SELECT max_runs,used,policy_digest,next_at FROM harness_automatic_continuations WHERE root_id=?`, b.RootBudgetID).Scan(&limit, &used, &digest, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return "queued", "", nil
	}
	if err != nil {
		return
	}
	if limit != spec.Automatic.Limit || digest != spec.Roles.Digest {
		return "blocked", "Saved root continuation policy differs from the task policy.", nil
	}
	if used >= limit {
		return "limit_reached", "The root automatic continuation limit has been reached.", nil
	}
	if time.Now().UnixNano() < next {
		return "deferred", "", nil
	}
	return "queued", "", nil
}

func setNotificationWorkTx(ctx context.Context, tx *sql.Tx, sessionID, notificationID, taskID, role, state, detail, runID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_notification_work(notification_id,session_id,task_id,recipient_role,state,detail,run_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(notification_id) DO UPDATE SET state=excluded.state,detail=excluded.detail,run_id=excluded.run_id`, notificationID, sessionID, taskID, role, state, detail, runID)
	return err
}

// Runs inside source/execution/input/budget admission. No auto quota or source
// can survive if any later write in that transaction fails.
func chargeAutomaticContinuationTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, n bt.Notification) error {
	automatic, _ := ctx.Value(automaticNotificationKey{}).(bool)
	if !automatic {
		return nil
	}
	state, _, err := notificationEligibilityTx(ctx, tx, b, spec, n)
	if err != nil {
		return err
	}
	if state != "queued" {
		return errAutomaticDeferred
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_automatic_continuations(root_id,session_id,policy_digest,max_runs,used,next_at) VALUES(?,?,?,?,0,0) ON CONFLICT DO NOTHING`, b.RootBudgetID, b.RootSession(), spec.Roles.Digest, spec.Automatic.Limit); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE harness_automatic_continuations SET used=used+1,next_at=? WHERE root_id=? AND policy_digest=? AND max_runs=? AND used<max_runs`, time.Now().Add(time.Second).UnixNano(), b.RootBudgetID, spec.Roles.Digest, spec.Automatic.Limit)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errAutomaticDeferred
	}
	return err
}

// Scan independent of UI handled flags. Persist classifications so blocked or
// disabled items cannot starve later roots. Revalidate every selected item in
// the actual continuation admission transaction after taking the session slot.
func (s *Service) nextAutomaticNotification(ctx context.Context, owner, sessionID string) (string, error) {
	if s.Background == nil {
		return "", nil
	}
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return "", err
	}
	var selected string
	err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		// Unvisited entries precede already inspected waits even when every
		// scan takes longer than their retry delay. Otherwise the first page
		// of private waits can indefinitely hide later root notifications.
		rows, err := tx.QueryContext(ctx, `SELECT i.notification_id,i.task_id,i.parent_session_id FROM harness_background_inbox i LEFT JOIN harness_background_tree t ON t.task_id=i.task_id LEFT JOIN harness_notification_work w ON w.notification_id=i.notification_id LEFT JOIN harness_continuation_sources c ON c.notification_id=i.notification_id LEFT JOIN harness_background_continuation_sources bc ON bc.notification_id=i.notification_id WHERE (i.parent_session_id=? OR t.root_session_id=?) AND c.run_id IS NULL AND bc.task_id IS NULL AND (w.state IS NULL OR w.state IN ('queued','waiting_resources') OR (w.state='waiting_parent' AND t.parent_task_id<>'')) AND (w.next_at IS NULL OR w.next_at<=?) ORDER BY COALESCE(w.next_at,0),i.seq LIMIT 100`, sessionID, sessionID, time.Now().UnixNano())
		if err != nil {
			return err
		}
		type candidate struct{ id, task, session string }
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err = rows.Scan(&c.id, &c.task, &c.session); err != nil {
				break
			}
			candidates = append(candidates, c)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, c := range candidates {
			n, err := readContinuationNotification(ctx, tx, c.session, c.id)
			if err != nil {
				if !blockedContinuationSource(err) {
					return err
				}
				if err = setNotificationWorkTx(ctx, tx, c.session, c.id, c.task, "", "blocked", "The saved notification requires review.", ""); err != nil {
					return err
				}
				continue
			}
			if !terminalBackgroundStatus(string(n.Kind)) && n.Kind != roleResultNotification {
				if err = setNotificationWorkTx(ctx, tx, c.session, c.id, c.task, "", "ignored", "Nonterminal notification.", ""); err != nil {
					return err
				}
				continue
			}
			b, spec, _, _, originErr := readContinuationOrigin(ctx, tx, c.session, c.task)
			if originErr == nil && b.RootSession() != sessionID {
				originErr = harness.ErrPermissionDenied
			}
			if originErr != nil && !blockedContinuationSource(originErr) {
				return originErr
			}
			state, detail := "blocked", "The saved task source or tool outcome requires review."
			if originErr == nil {
				state, detail, err = notificationEligibilityTx(ctx, tx, b, spec, n)
				if err != nil {
					return err
				}
				if state == "queued" && c.session != sessionID && s.BackgroundContinuation == nil {
					state, detail = "waiting_parent", "The original background parent context is saved; its continuation executor is not yet available."
				}
				if state == "queued" && s.CheckContinuationResources != nil {
					resourceErr := s.CheckContinuationResources(ctx, tx, b, spec)
					if errors.Is(resourceErr, harness.ErrResourcesWaiting) || errors.Is(resourceErr, harness.ErrResourcesIncompatible) {
						state, detail = "waiting_resources", "The saved parent MCP resources require reconnection and explicit authorization."
					} else if resourceErr != nil {
						return resourceErr
					}
				}
			}
			recipient := b.ParentInvocationID
			if originErr == nil && n.Kind == roleResultNotification {
				if recipient, _, err = notificationRecipientTx(ctx, tx, b, spec, n); err != nil {
					if state != "blocked" {
						return err
					}
					// A corrupt result has no trusted recipient. Keep it blocked
					// without preventing later roots from receiving their results.
					recipient = ""
				}
			}
			if state == "deferred" {
				if err = setNotificationWorkTx(ctx, tx, c.session, c.id, c.task, recipient, "queued", "Waiting for the root continuation retry time.", ""); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `UPDATE harness_notification_work SET next_at=(SELECT next_at FROM harness_automatic_continuations WHERE root_id=?) WHERE notification_id=?`, b.RootBudgetID, c.id); err != nil {
					return err
				}
				continue
			}
			if err = setNotificationWorkTx(ctx, tx, c.session, c.id, c.task, recipient, state, detail, ""); err != nil {
				return err
			}
			if state == "waiting_parent" && c.session != sessionID || state == "waiting_resources" {
				// Revisit a parent that is still completing without letting a
				// large private inbox starve later roots in this bounded scan.
				if _, err = tx.ExecContext(ctx, `UPDATE harness_notification_work SET next_at=? WHERE notification_id=?`, time.Now().Add(time.Second).UnixNano(), c.id); err != nil {
					return err
				}
			}
			if state == "queued" {
				selected = c.id
				break
			}
		}
		return nil
	})
	return selected, err
}

func (s *Service) processAutomaticNotification(ctx context.Context, owner, sessionID, id string, handlers harness.SessionUpdates) (harness.RunResult, error) {
	ctx, release, err := s.runQueue.tryAutomatic(ctx)
	if err != nil {
		return harness.RunResult{}, err
	}
	defer release()
	ctx = context.WithValue(ctx, automaticNotificationKey{}, true)
	result, runErr := s.ProcessBackgroundNotification(ctx, owner, sessionID, id, handlers.Events, handlers.Permission)
	if errors.Is(runErr, harness.ErrResourcesWaiting) || errors.Is(runErr, harness.ErrResourcesIncompatible) {
		persistErr := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE harness_notification_work SET state='waiting_resources',detail='The saved MCP configuration and tool schemas require a compatible, explicitly authorized connection.',next_at=? WHERE notification_id=? AND state<>'admitted'`, time.Now().Add(time.Second).UnixNano(), id)
			return err
		})
		return result, errors.Join(runErr, persistErr)
	}
	if runErr == nil || result.Execution != nil || ctx.Err() != nil || errors.Is(runErr, harness.ErrBusy) || errors.Is(runErr, harness.ErrExecutionWaitingInput) || errors.Is(runErr, errAutomaticDeferred) {
		return result, runErr
	}
	// Admission failures did not consume the root quota. Bound these retries
	// separately; source/budget errors are immediately inspectable, not retried.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = withExecutionTransaction(persist, s.Store, func(tx *sql.Tx) error {
		var attempts int
		if err := tx.QueryRowContext(persist, `SELECT attempts FROM harness_notification_work WHERE notification_id=?`, id).Scan(&attempts); err != nil {
			return err
		}
		attempts++
		state, detail := "queued", "Automatic admission failed; retry pending."
		var limit *budget.LimitError
		if attempts >= 3 || errors.Is(runErr, harness.ErrExecutionUnresumable) || errors.Is(runErr, harness.ErrReconciliationRequired) || errors.As(runErr, &limit) && !limit.Temporary {
			state, detail = "blocked", "Automatic admission requires review or budget recovery."
		}
		delay := time.Second
		if attempts > 1 {
			delay = 5 * time.Second
		}
		_, err := tx.ExecContext(persist, `UPDATE harness_notification_work SET state=?,detail=?,attempts=?,next_at=? WHERE notification_id=? AND state='queued'`, state, detail, attempts, time.Now().Add(delay).UnixNano(), id)
		return err
	})
	return result, errors.Join(runErr, err)
}
