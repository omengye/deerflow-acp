package automation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrConflict = errors.New("schedule changed; refresh before retrying")
	ErrBusy     = errors.New("schedule occurrence is running; pause future fires or wait for completion")
	ErrNotFound = errors.New("schedule no longer exists")
	ErrInvalid  = errors.New("invalid schedule change")
)

// Timing deliberately excludes prompts, grants, workspaces and notification targets.
type Timing struct {
	Kind         string    `json:"kind"`
	At           time.Time `json:"at,omitempty"`
	EverySeconds int64     `json:"every_seconds,omitempty"`
	Cron         string    `json:"cron,omitempty"`
	Timezone     string    `json:"timezone"`
}

type Preview struct {
	Next      time.Time `json:"next"`
	NextLocal string    `json:"next_local"`
}

func (t Timing) apply(spec *Spec) {
	spec.Kind, spec.At, spec.EverySeconds, spec.Cron, spec.Timezone = t.Kind, t.At, t.EverySeconds, t.Cron, t.Timezone
	if spec.Timezone == "" {
		spec.Timezone = "Asia/Shanghai"
	}
}

func (t Timing) Preview(now time.Time) (Preview, error) {
	var spec Spec
	t.apply(&spec)
	next, err := spec.Next(now)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if next.IsZero() || next.UnixMilli() <= now.UnixMilli() {
		return Preview{}, fmt.Errorf("%w: time must be in the future", ErrInvalid)
	}
	return Preview{Next: next, NextLocal: localTime(next, spec.Timezone)}, nil
}

func localTime(t time.Time, zone string) string {
	if t.IsZero() {
		return ""
	}
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return t.UTC().Format(time.RFC3339)
	}
	return t.In(loc).Format(time.RFC3339)
}

func migrateSchedules(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(harness_schedules)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&cid, &name, &kind, &notnull, &value, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if !columns["revision"] {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE harness_schedules ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`); err != nil {
			return err
		}
	}
	if !columns["pause_reason"] {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE harness_schedules ADD COLUMN pause_reason TEXT NOT NULL DEFAULT '';
UPDATE harness_schedules SET pause_reason=CASE
WHEN (SELECT status FROM harness_schedule_occurrences o WHERE o.schedule_id=harness_schedules.id ORDER BY due_ms DESC,rowid DESC LIMIT 1) IN ('unknown','needs_attention') THEN 'needs_attention'
WHEN next_ms=0 THEN 'exhausted' ELSE 'manual' END WHERE paused=1;`); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_schedule_requests(request_key TEXT PRIMARY KEY,schedule_id TEXT NOT NULL,spec BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_schedule_sources(session_id TEXT PRIMARY KEY,source BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_schedule_run_sources(run_id TEXT PRIMARY KEY,source BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_schedule_buzz_grants(account TEXT NOT NULL,workspace TEXT NOT NULL,policy BLOB NOT NULL,version TEXT NOT NULL,expires_ms INTEGER NOT NULL,PRIMARY KEY(account,workspace));
CREATE TABLE IF NOT EXISTS harness_schedule_owner_links(principal TEXT PRIMARY KEY,owner TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS harness_schedule_deliveries(id TEXT PRIMARY KEY,schedule_id TEXT NOT NULL,target BLOB NOT NULL,source BLOB NOT NULL,account TEXT NOT NULL,workspace TEXT NOT NULL,text TEXT NOT NULL,status TEXT NOT NULL,updated_ms INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS harness_schedule_delivery_pending ON harness_schedule_deliveries(account,workspace,status,updated_ms);
CREATE INDEX IF NOT EXISTS harness_schedule_latest ON harness_schedule_occurrences(schedule_id,due_ms DESC);
CREATE INDEX IF NOT EXISTS harness_schedule_active ON harness_schedule_occurrences(schedule_id) WHERE status='running'`)
	return err
}

const scheduleSelect = `SELECT s.spec,s.paused,s.next_ms,s.revision,s.pause_reason,EXISTS(SELECT 1 FROM harness_schedule_occurrences WHERE schedule_id=s.id AND status='running'),
COALESCE(o.id,''),COALESCE(o.due_ms,0),COALESCE(o.status,''),COALESCE(o.session_id,''),COALESCE(substr(o.text,1,512),''),COALESCE(o.is_read,0)
FROM harness_schedules s LEFT JOIN harness_schedule_occurrences o ON o.rowid=(SELECT rowid FROM harness_schedule_occurrences WHERE schedule_id=s.id ORDER BY due_ms DESC,rowid DESC LIMIT 1)`

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var v Schedule
	var raw []byte
	var ms, due int64
	var last Occurrence
	err := row.Scan(&raw, &v.Paused, &ms, &v.Revision, &v.PauseReason, &v.Active, &last.ID, &due, &last.Outcome.Status, &last.Outcome.SessionID, &last.Outcome.Text, &last.Read)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(raw, &v.Spec); err != nil {
		return v, err
	}
	// Legacy omitted grants retain the dispatcher's historical read-only fallback.
	if v.Spec.Approval == "" {
		v.Spec.Approval = "read_only"
	}
	if ms != 0 {
		v.Next = time.UnixMilli(ms).UTC()
	}
	v.NextLocal = localTime(v.Next, v.Spec.Timezone)
	if last.ID != "" {
		last.ScheduleID, last.Due = v.Spec.ID, time.UnixMilli(due).UTC()
		v.Last = &last
	}
	return v, nil
}

func checkedSchedule(ctx context.Context, tx *sql.Tx, id string, revision int64) (Schedule, error) {
	if id == "" || revision < 1 {
		return Schedule{}, fmt.Errorf("%w: id and expected_revision are required", ErrInvalid)
	}
	v, err := scanSchedule(tx.QueryRowContext(ctx, scheduleSelect+` WHERE s.id=?`, id))
	if err != nil {
		return v, err
	}
	if v.Revision != revision {
		return v, ErrConflict
	}
	return v, nil
}

func (s *Scheduler) Reschedule(ctx context.Context, id string, revision int64, timing Timing, rearm bool) (Schedule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	v, err := checkedSchedule(ctx, tx, id, revision)
	if err != nil {
		return Schedule{}, err
	}
	if v.Active {
		return Schedule{}, ErrBusy
	}
	if rearm && (v.Spec.Kind != "once" || !v.Next.IsZero() || timing.Kind != "once") {
		return Schedule{}, fmt.Errorf("%w: rearm requires an ended one-time schedule and a new one-time date", ErrInvalid)
	}
	preview, err := timing.Preview(time.Now())
	if err != nil {
		return Schedule{}, err
	}
	timing.apply(&v.Spec)
	if rearm {
		v.Paused, v.PauseReason = false, ""
	} else if v.PauseReason == "exhausted" {
		v.PauseReason = "manual"
	}
	body, err := json.Marshal(v.Spec)
	if err != nil {
		return Schedule{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_schedules SET spec=?,next_ms=?,paused=?,pause_reason=?,revision=revision+1 WHERE id=?`, body, preview.Next.UnixMilli(), v.Paused, v.PauseReason, id); err != nil {
		return Schedule{}, err
	}
	v, err = scanSchedule(tx.QueryRowContext(ctx, scheduleSelect+` WHERE s.id=?`, id))
	if err != nil {
		return Schedule{}, err
	}
	return v, tx.Commit()
}
