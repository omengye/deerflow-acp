package background

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

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
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)", n.ID, n.SessionID, n.TaskID, data)
	if err != nil {
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
	rows, err := s.store.DB().QueryContext(ctx, "SELECT seq,payload FROM harness_background_inbox WHERE parent_session_id=? AND handled=0 AND seq>? ORDER BY seq LIMIT ?", actor.SessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []harness.BackgroundNotification
	for rows.Next() {
		var seq int64
		var data []byte
		if err = rows.Scan(&seq, &data); err != nil {
			return nil, err
		}
		var n bt.Notification
		if err = json.Unmarshal(data, &n); err != nil {
			return nil, err
		}
		result = append(result, harness.BackgroundNotification{Sequence: seq, ID: n.ID, TaskID: n.TaskID, SessionID: n.SessionID, Kind: string(n.Kind), TaskVersion: n.Version, Data: append([]byte(nil), n.Data...), CreatedAt: n.CreatedAt})
	}
	return result, rows.Err()
}

// AcknowledgeInbox marks a delivered UI/input item handled. For model input,
// the host must first durably admit an idempotent input keyed by notification ID.
func (s *Service) AcknowledgeInbox(ctx context.Context, actor harness.TaskActor, id string) error {
	if err := s.authorize(ctx, actor); err != nil {
		return err
	}
	r, err := s.store.DB().ExecContext(ctx, "UPDATE harness_background_inbox SET handled=1 WHERE notification_id=? AND parent_session_id=?", id, actor.SessionID)
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
	return nil
}
