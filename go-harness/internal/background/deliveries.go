package background

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// TaskDelivery is trusted host evidence, after the runtime checks the live
// execution lease, task ancestry, terminal version and uncertainty frontier.
type TaskDelivery struct {
	TaskID, RecipientRole, RunID, RootBudgetID, SessionID string
	Version                                               int64
}

// RecordTaskDeliveryTx is part of successful execution/checkpoint publication.
// It updates existing inbox projections without changing UI read or outbox ACK.
// A later outbox arrival applies the same projection in acceptNotification.
func RecordTaskDeliveryTx(ctx context.Context, tx *sql.Tx, d TaskDelivery) error {
	if d.TaskID == "" || d.Version < 1 || d.RunID == "" || d.RootBudgetID == "" || d.SessionID == "" {
		return harness.ErrInvalidInput
	}
	b, _, err := loadBinding(ctx, tx, d.TaskID)
	if err != nil {
		return err
	}
	if b.RootBudgetID != d.RootBudgetID {
		return harness.ErrTaskOriginConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_task_deliveries(task_id,task_version,recipient_role,run_id,root_id,session_id) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, d.TaskID, d.Version, d.RecipientRole, d.RunID, d.RootBudgetID, d.SessionID); err != nil {
		return err
	}
	if d.RecipientRole != notificationRecipient(b) || d.SessionID != b.ParentSessionID {
		// An authorized ancestor may inspect results without consuming the
		// notification intended for the task's original, private parent role.
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM harness_background_inbox WHERE task_id=? AND parent_session_id=?`, d.TaskID, b.ParentSessionID)
	if err != nil {
		return err
	}
	var notifications []bt.Notification
	for rows.Next() {
		var data []byte
		var n bt.Notification
		if err = rows.Scan(&data); err != nil {
			break
		}
		if err = json.Unmarshal(data, &n); err != nil {
			break
		}
		notifications = append(notifications, n)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, n := range notifications {
		if err = projectConsumedNotificationTx(ctx, tx, b, n); err != nil {
			return err
		}
	}
	return nil
}

func notificationRecipient(b Binding) string {
	if b.ParentInvocationID != "" {
		return b.ParentInvocationID
	}
	if b.ParentTaskID != "" {
		return "background/" + b.ParentTaskID
	}
	return ""
}

func projectConsumedNotificationTx(ctx context.Context, tx *sql.Tx, b Binding, n bt.Notification) error {
	if n.TaskID != b.TaskID || n.SessionID != b.ParentSessionID {
		return harness.ErrTaskOriginConflict
	}
	switch n.Kind {
	case bt.NotificationCompleted, bt.NotificationFailed, bt.NotificationCanceled:
	default:
		return nil
	}
	role := notificationRecipient(b)
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT run_id FROM harness_task_deliveries WHERE task_id=? AND task_version>=? AND recipient_role=? AND root_id=? AND session_id=? ORDER BY task_version LIMIT 1`, b.TaskID, n.Version, role, b.RootBudgetID, b.ParentSessionID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_notification_work(notification_id,session_id,task_id,recipient_role,state,detail,run_id) VALUES(?,?,?,?,'consumed',?,?)
ON CONFLICT(notification_id) DO UPDATE SET state='consumed',detail=excluded.detail,run_id=excluded.run_id,next_at=0 WHERE harness_notification_work.state<>'admitted'`, n.ID, n.SessionID, n.TaskID, role, "The saved parent execution already received this task result.", runID)
	return err
}
