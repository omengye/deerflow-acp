package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type scheduleDelivery struct {
	ID         string                        `json:"id"`
	ScheduleID string                        `json:"schedule_id"`
	Target     automation.NotificationTarget `json:"target"`
	Text       string                        `json:"text"`
	Status     string                        `json:"status"`
	source     automation.Source
}

func (c *Client) enqueueScheduleNotification(ctx context.Context, tx *sql.Tx, spec automation.Spec, occ automation.Occurrence) error {
	if spec.Notify == nil || spec.Notify.Channel != "buzz" {
		return c.channels.EnqueueNotificationTx(ctx, tx, spec, occ)
	}
	raw, err := json.Marshal(spec.Notify)
	if err != nil {
		return err
	}
	if spec.Source == nil {
		return errors.New("Buzz delivery requires a trusted schedule source")
	}
	source, err := json.Marshal(spec.Source)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Scheduled task: %s\nStatus: %s\nTask: %s\nSession: %s\n%s", spec.Name, occ.Outcome.Status, spec.ID, occ.Outcome.SessionID, occ.Outcome.Text)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_schedule_deliveries(id,schedule_id,target,source,account,workspace,text,status,updated_ms) VALUES(?,?,?,?,?,?,?,'ready',?)`, occ.ID, spec.ID, raw, source, spec.Notify.Account, spec.Workspace, text, time.Now().UnixMilli())
	return err
}

func (c *Client) managementScheduleDelivery(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	var in struct {
		Account   string `json:"account"`
		Workspace string `json:"workspace"`
		ID        string `json:"id"`
		Status    string `json:"status"`
	}
	if err := decodeManagement(r, &in, "account", "workspace", "id", "status"); err != nil {
		return nil, err
	}
	workspace, err := session.NormalizeWorkspace(in.Workspace)
	if err != nil {
		return nil, managementInvalid("choose an existing workspace directory")
	}
	in.Workspace = workspace
	db := c.store.DB()
	if r.Operation == "schedule.buzz.poll" {
		// A send whose acknowledgement was lost is quarantined, never replayed.
		if _, err := db.ExecContext(ctx, `UPDATE harness_schedule_deliveries SET status='unknown' WHERE status='sending' AND updated_ms<?`, time.Now().Add(-5*time.Minute).UnixMilli()); err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, `SELECT id,schedule_id,target,source,text,status FROM harness_schedule_deliveries WHERE status='ready' AND account=? AND workspace=? ORDER BY updated_ms,id LIMIT 200`, in.Account, in.Workspace)
		if err != nil {
			return nil, err
		}
		var pending []scheduleDelivery
		for rows.Next() {
			var v scheduleDelivery
			var raw, source []byte
			if err = rows.Scan(&v.ID, &v.ScheduleID, &raw, &source, &v.Text, &v.Status); err != nil {
				rows.Close()
				return nil, err
			}
			if err = json.Unmarshal(raw, &v.Target); err != nil {
				rows.Close()
				return nil, err
			}
			if err = json.Unmarshal(source, &v.source); err != nil {
				rows.Close()
				return nil, err
			}
			if v.Target.Account == in.Account {
				pending = append(pending, v)
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
		out := []scheduleDelivery{}
		for _, v := range pending {
			if c.authorizeScheduleSource(ctx, v.source, true) == nil {
				out = append(out, v)
			} else {
				if _, err = db.ExecContext(ctx, `UPDATE harness_schedule_deliveries SET status='blocked' WHERE id=? AND status='ready'`, v.ID); err != nil {
					return nil, err
				}
			}
			if len(out) >= 20 {
				break
			}
		}
		return out, nil
	}
	var v scheduleDelivery
	var raw, source []byte
	err = db.QueryRowContext(ctx, `SELECT id,schedule_id,target,source,text,status FROM harness_schedule_deliveries WHERE id=? AND account=? AND workspace=?`, in.ID, in.Account, in.Workspace).Scan(&v.ID, &v.ScheduleID, &raw, &source, &v.Text, &v.Status)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &v.Target); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(source, &v.source); err != nil {
		return nil, err
	}
	if v.Target.Account != in.Account || v.source.Workspace != in.Workspace {
		return nil, harness.ErrPermissionDenied
	}
	if err = c.authorizeScheduleSource(ctx, v.source, true); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "schedule.buzz.claim":
		if v.Status != "ready" {
			return nil, automation.ErrConflict
		}
		_, err = db.ExecContext(ctx, `UPDATE harness_schedule_deliveries SET status='sending',updated_ms=? WHERE id=? AND status='ready'`, time.Now().UnixMilli(), in.ID)
		v.Status = "sending"
		return v, err
	case "schedule.buzz.ack":
		if in.Status != "delivered" && in.Status != "unknown" && in.Status != "ready" {
			return nil, automation.ErrInvalid
		}
		if v.Status == in.Status {
			return map[string]any{"saved": true}, nil
		}
		if v.Status != "sending" {
			return nil, automation.ErrConflict
		}
		_, err = db.ExecContext(ctx, `UPDATE harness_schedule_deliveries SET status=?,updated_ms=? WHERE id=? AND status='sending'`, in.Status, time.Now().UnixMilli(), in.ID)
		return map[string]any{"saved": err == nil}, err
	default:
		return nil, managementInvalid("unknown schedule source operation")
	}
}

// One bounded read enriches visible receipts; render paths never perform I/O.
func (c *Client) scheduleNotifications(ctx context.Context, entries []*automation.Occurrence) error {
	if len(entries) == 0 {
		return nil
	}
	ids, lookup := make([]any, 0, len(entries)), map[string]*automation.Occurrence{}
	for _, entry := range entries {
		if entry != nil {
			ids = append(ids, entry.ID)
			lookup[entry.ID] = entry
		}
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := append([]any{}, ids...)
	for _, id := range ids {
		args = append(args, "schedule-"+id.(string))
	}
	rows, err := c.store.DB().QueryContext(ctx, `SELECT id,status FROM harness_schedule_deliveries WHERE id IN (`+placeholders+`) UNION ALL SELECT substr(id,10),status FROM harness_channel_messages WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err = rows.Scan(&id, &status); err != nil {
			return err
		}
		if entry := lookup[id]; entry != nil {
			entry.Notification = status
		}
	}
	return rows.Err()
}

func (c *Client) scheduleHistory(ctx context.Context, id string) ([]automation.Occurrence, error) {
	items, err := c.scheduler.History(ctx, id)
	if err != nil {
		return nil, err
	}
	entries := make([]*automation.Occurrence, 0, len(items))
	for i := range items {
		entries = append(entries, &items[i])
	}
	return items, c.scheduleNotifications(ctx, entries)
}

func (c *Client) scheduleList(ctx context.Context) ([]automation.Schedule, error) {
	items, err := c.scheduler.List(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]*automation.Occurrence, 0, len(items))
	for i := range items {
		entries = append(entries, items[i].Last)
	}
	return items, c.scheduleNotifications(ctx, entries)
}
