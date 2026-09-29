package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

func (s *TaskStore) enqueueLifecycle(ctx context.Context, tx *sql.Tx, task *bt.Task, kind bt.NotificationKind) error {
	if kind == "" || task.Spec.SessionID == "" || (kind != bt.NotificationTaskCreated && !task.Spec.NotifySession) {
		return nil
	}
	n := &bt.Notification{ID: fmt.Sprintf("%s:%d:%s", task.Spec.ID, task.Version, kind), TaskID: task.Spec.ID, SessionID: task.Spec.SessionID, Version: task.Version, Kind: kind, CreatedAt: s.now().UTC()}
	return insertNotification(ctx, tx, n)
}

func insertNotification(ctx context.Context, tx *sql.Tx, n *bt.Notification) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO eino_task_notifications(id,task_id,payload) VALUES(?,?,?)", n.ID, n.TaskID, data)
	return err
}

func (s *TaskStore) EnqueueTaskNotification(ctx context.Context, id string, attempt int64, req *bt.NotifyParentRequest) error {
	if id == "" || attempt <= 0 || req == nil || req.EventID == "" || len(req.EventID) > 1024 || req.Kind == "" || len(req.Kind) > 64 || len(req.Data) > 256<<10 {
		return errors.New("backgroundtask: invalid application notification request")
	}
	if strings.HasPrefix(string(req.Kind), "eino.") {
		return errors.New("backgroundtask: notification kind is reserved")
	}
	switch req.Kind {
	case bt.NotificationTaskCreated, bt.NotificationWaitingInput, bt.NotificationCompleted, bt.NotificationFailed, bt.NotificationCanceled:
		return errors.New("backgroundtask: notification kind is reserved")
	}
	_, err := s.withTask(ctx, id, false, func(tx *sql.Tx, r *taskRecord) error {
		if err := s.authorizeAttempt(r, attempt); err != nil {
			return err
		}
		if r.task.Spec.SessionID == "" {
			return bt.ErrNotificationUnavailable
		}
		var kind string
		var data []byte
		err := tx.QueryRowContext(ctx, "SELECT kind,data FROM eino_notification_replays WHERE task_id=? AND event_id=?", id, req.EventID).Scan(&kind, &data)
		if err == nil {
			if kind != string(req.Kind) || !bytes.Equal(data, req.Data) {
				return bt.ErrNotificationEventIDConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO eino_notification_replays(task_id,event_id,kind,data) VALUES(?,?,?,?)", id, req.EventID, string(req.Kind), req.Data)
		if err != nil {
			return err
		}
		n := &bt.Notification{
			ID:     "eino.custom:" + base64.RawURLEncoding.EncodeToString([]byte(id)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(req.EventID)) + ":application-event",
			TaskID: id, SessionID: r.task.Spec.SessionID, Version: r.task.Version, Kind: req.Kind, Data: req.Data, CreatedAt: s.now().UTC(),
		}
		return insertNotification(ctx, tx, n)
	})
	return err
}

func (s *TaskStore) Receive(ctx context.Context, req *bt.ReceiveNotificationsRequest) (*bt.ReceiveNotificationsResult, error) {
	if req == nil || req.LeaseDuration <= 0 {
		return nil, errors.New("backgroundtask: positive notification lease is required")
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.now()
	expires := now.Add(req.LeaseDuration).UnixNano()
	rows, err := tx.QueryContext(ctx, "SELECT seq,payload FROM eino_task_notifications WHERE lease_expires_at<=? ORDER BY seq LIMIT ?", now.UnixNano(), pageLimit(req.Limit))
	if err != nil {
		return nil, err
	}
	var seqs []int64
	result := &bt.ReceiveNotificationsResult{}
	for rows.Next() {
		var seq int64
		var data []byte
		if err = rows.Scan(&seq, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var n bt.Notification
		if err = json.Unmarshal(data, &n); err != nil {
			rows.Close()
			return nil, err
		}
		receipt := make([]byte, 32)
		if _, err = rand.Read(receipt); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, seq)
		result.Deliveries = append(result.Deliveries, bt.NotificationDelivery{Record: n, Receipt: receipt})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i, seq := range seqs {
		if _, err = tx.ExecContext(ctx, "UPDATE eino_task_notifications SET receipt=?,lease_expires_at=? WHERE seq=?", []byte(result.Deliveries[i].Receipt), expires, seq); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *TaskStore) Ack(ctx context.Context, receipt bt.NotificationReceipt) error {
	if len(receipt) == 0 {
		return bt.ErrNotFound
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq, expires int64
	err = tx.QueryRowContext(ctx, "SELECT seq,lease_expires_at FROM eino_task_notifications WHERE receipt=?", []byte(receipt)).Scan(&seq, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return bt.ErrNotFound
	}
	if err != nil {
		return err
	}
	if expires <= s.now().UnixNano() {
		return bt.ErrLeaseLost
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM eino_task_notifications WHERE seq=?", seq); err != nil {
		return err
	}
	return tx.Commit()
}
