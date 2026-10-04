package background

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Shared host delivery metadata; independent of native outbox ACK and UI read.
func MigrateNotificationWork(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_notification_work (
 notification_id TEXT PRIMARY KEY,session_id TEXT NOT NULL,task_id TEXT NOT NULL,
 recipient_role TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL DEFAULT 0,run_id TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS harness_notification_work_session ON harness_notification_work(session_id,state,next_at);
CREATE TABLE IF NOT EXISTS harness_task_deliveries (
 task_id TEXT NOT NULL,task_version INTEGER NOT NULL,recipient_role TEXT NOT NULL,
 run_id TEXT NOT NULL,root_id TEXT NOT NULL,session_id TEXT NOT NULL,
 PRIMARY KEY(task_id,task_version,recipient_role));`)
	return err
}

// DeliverNotifications commits idempotent parent inbox entries before acking
// the leased native outbox. A crash between commit and Ack replays safely.
func (s *Service) DeliverNotifications(ctx context.Context) error {
	batch, err := s.tasks.Receive(ctx, &bt.ReceiveNotificationsRequest{LeaseDuration: s.config.NotificationLease, Limit: 100})
	if err != nil {
		return err
	}
	var failures []error
	for _, delivery := range batch.Deliveries {
		if err = s.acceptNotification(ctx, delivery.Record); err != nil {
			failures = append(failures, err)
			continue
		}
		if err = s.tasks.Ack(ctx, delivery.Receipt); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) acceptNotification(ctx context.Context, n bt.Notification) error {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, _, err := loadBinding(ctx, tx, n.TaskID)
	if err != nil {
		return err
	}
	if n.ID == "" || n.SessionID != b.ParentSessionID {
		return harness.ErrPermissionDenied
	}
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	var prior []byte
	err = tx.QueryRowContext(ctx, "SELECT payload FROM harness_background_inbox WHERE notification_id=?", n.ID).Scan(&prior)
	if err == nil {
		if !bytes.Equal(prior, data) {
			return bt.ErrNotificationEventIDConflict
		}
		if err = projectConsumedNotificationTx(ctx, tx, b, n); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)", n.ID, n.SessionID, n.TaskID, data)
	if err != nil {
		return err
	}
	if err = projectConsumedNotificationTx(ctx, tx, b, n); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ListInbox(ctx context.Context, actor harness.TaskActor, after int64, limit int) ([]harness.BackgroundNotification, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, harness.ErrInvalidInput
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := "SELECT i.seq,i.payload,w.state,w.detail,w.attempts,w.next_at,w.run_id,w.recipient_role FROM harness_background_inbox i LEFT JOIN harness_notification_work w ON w.notification_id=i.notification_id WHERE i.parent_session_id=? AND i.handled=0 AND i.seq>? ORDER BY i.seq LIMIT ?"
	args := []any{actor.SessionID, after, limit}
	if scope, ok := ScopeFromContext(ctx); ok {
		// Reusing a child session does not transfer the previous task's inbox
		// ownership to a new invocation using the same session history.
		query = `SELECT i.seq,i.payload,w.state,w.detail,w.attempts,w.next_at,w.run_id,w.recipient_role FROM harness_background_inbox i JOIN harness_background_tree t ON t.task_id=i.task_id LEFT JOIN harness_notification_work w ON w.notification_id=i.notification_id WHERE i.parent_session_id=? AND t.parent_task_id=? AND i.handled=0 AND i.seq>? ORDER BY i.seq LIMIT ?`
		args = []any{actor.SessionID, scope.Binding.TaskID, after, limit}
	}
	rows, err := s.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []harness.BackgroundNotification
	for rows.Next() {
		var seq int64
		var data []byte
		var state, detail, runID, role sql.NullString
		var attempts, next sql.NullInt64
		if err = rows.Scan(&seq, &data, &state, &detail, &attempts, &next, &runID, &role); err != nil {
			return nil, err
		}
		var n bt.Notification
		if err = json.Unmarshal(data, &n); err != nil {
			return nil, err
		}
		item := harness.BackgroundNotification{Sequence: seq, ID: n.ID, TaskID: n.TaskID, SessionID: n.SessionID, Kind: string(n.Kind), TaskVersion: n.Version, Data: append([]byte(nil), n.Data...), CreatedAt: n.CreatedAt}
		if state.Valid {
			item.Delivery = &harness.NotificationDelivery{Status: state.String, Detail: detail.String, Attempts: int(attempts.Int64), RunID: runID.String, RecipientRole: role.String}
			if next.Int64 > 0 {
				when := time.Unix(0, next.Int64).UTC()
				item.Delivery.RetryAfter = &when
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// AcknowledgeInbox marks a delivered UI/input item handled. For model input,
// the host must first durably admit an idempotent input keyed by notification ID.
func (s *Service) AcknowledgeInbox(ctx context.Context, actor harness.TaskActor, id string) error {
	if err := s.authorize(ctx, actor); err != nil {
		return err
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := "UPDATE harness_background_inbox SET handled=1 WHERE notification_id=? AND parent_session_id=?"
	args := []any{id, actor.SessionID}
	if scope, ok := ScopeFromContext(ctx); ok {
		if err = s.CheckEffectTx(ctx, tx, scope); err != nil {
			return err
		}
		query += " AND task_id IN (SELECT task_id FROM harness_background_tree WHERE parent_task_id=?)"
		args = append(args, scope.Binding.TaskID)
	}
	r, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return harness.ErrNotFound
	}
	return tx.Commit()
}
