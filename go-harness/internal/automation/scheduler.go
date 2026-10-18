// Package automation owns calendar admission, never execution or tool policy.
package automation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
)

type Spec struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Workspace    string              `json:"workspace"`
	Prompt       string              `json:"prompt"`
	Kind         string              `json:"kind"`
	At           time.Time           `json:"at,omitempty"`
	EverySeconds int64               `json:"every_seconds,omitempty"`
	Cron         string              `json:"cron,omitempty"`
	Timezone     string              `json:"timezone"`
	Approval     string              `json:"approval"`
	NotifyWeChat string              `json:"notify_wechat,omitempty"`
	Notify       *NotificationTarget `json:"notify,omitempty"`
	Source       *Source             `json:"source,omitempty"`
}
type NotificationTarget struct {
	Channel      string `json:"channel"`
	Account      string `json:"account"`
	Conversation string `json:"conversation"`
	Sender       string `json:"sender"`
	ReplyTo      string `json:"reply_to,omitempty"`
}
type Schedule struct {
	Spec        Spec        `json:"spec"`
	Paused      bool        `json:"paused"`
	Next        time.Time   `json:"next"`
	Revision    int64       `json:"revision"`
	PauseReason string      `json:"pause_reason"`
	NextLocal   string      `json:"next_local"`
	Active      bool        `json:"active"`
	Last        *Occurrence `json:"last,omitempty"`
}
type Outcome struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Text      string `json:"text"`
	// Attention is a host-authored, credential-free channel recovery notice.
	Attention string `json:"attention,omitempty"`
}
type Occurrence struct {
	ID           string    `json:"id"`
	ScheduleID   string    `json:"schedule_id"`
	Due          time.Time `json:"due"`
	Outcome      Outcome   `json:"outcome"`
	Read         bool      `json:"read"`
	Notification string    `json:"notification,omitempty"`
}
type Runner func(context.Context, Spec, func(string) error) (Outcome, error)
type Scheduler struct {
	db        *sql.DB
	run       Runner
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	tickMu    sync.Mutex
	lastError string
	NotifyTx  func(context.Context, *sql.Tx, Spec, Occurrence) error
}

func New(ctx context.Context, db *sql.DB, run Runner) (*Scheduler, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_schedules(id TEXT PRIMARY KEY,spec BLOB NOT NULL,paused INTEGER NOT NULL,next_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS harness_schedule_occurrences(id TEXT PRIMARY KEY,schedule_id TEXT NOT NULL,due_ms INTEGER NOT NULL,status TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',text TEXT NOT NULL DEFAULT '',is_read INTEGER NOT NULL DEFAULT 0,UNIQUE(schedule_id,due_ms));
CREATE INDEX IF NOT EXISTS harness_schedule_due ON harness_schedules(paused,next_ms);`)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = migrateSchedules(ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_schedules SET paused=1,pause_reason='interrupted',revision=revision+1 WHERE id IN (SELECT schedule_id FROM harness_schedule_occurrences WHERE status='running')`); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_schedule_occurrences SET status='unknown',text='Daemon stopped before terminal receipt; review the linked session. This occurrence will not be replayed.' WHERE status='running'`); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Scheduler{db: db, run: run}, nil
}

func (s Spec) Next(after time.Time) (time.Time, error) {
	if s.Timezone == "" {
		s.Timezone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, errors.New("invalid schedule timezone")
	}
	switch s.Kind {
	case "once":
		if s.Cron != "" || s.EverySeconds != 0 {
			return time.Time{}, errors.New("once accepts only at")
		}
		if s.At.IsZero() {
			return time.Time{}, errors.New("once requires an RFC3339 at timestamp")
		}
		if !s.At.After(after) {
			return time.Time{}, nil
		}
		return s.At.UTC(), nil
	case "interval":
		if s.Cron != "" || !s.At.IsZero() {
			return time.Time{}, errors.New("interval accepts only every_seconds")
		}
		if s.EverySeconds < 60 || s.EverySeconds > 31536000 {
			return time.Time{}, errors.New("every_seconds must be 60..31536000")
		}
		return after.Add(time.Duration(s.EverySeconds) * time.Second).UTC(), nil
	case "cron":
		if s.EverySeconds != 0 || !s.At.IsZero() {
			return time.Time{}, errors.New("cron accepts only cron and timezone")
		}
		if strings.Contains(s.Cron, "TZ=") || len(s.Cron) > 128 {
			return time.Time{}, errors.New("use the timezone field, not a cron TZ prefix")
		}
		p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		expr, err := p.Parse(s.Cron)
		if err != nil {
			return time.Time{}, err
		}
		next := expr.Next(after.In(loc))
		if next.IsZero() {
			return next, errors.New("cron has no upcoming occurrence")
		}
		return next.UTC(), nil
	default:
		return time.Time{}, errors.New("kind must be once, interval or cron")
	}
}

func (s *Scheduler) Create(ctx context.Context, spec Spec) (Schedule, error) {
	return s.create(ctx, spec, "")
}

func (s *Scheduler) CreateIdempotent(ctx context.Context, spec Spec, key string) (Schedule, error) {
	if key == "" || len(key) > 512 {
		return Schedule{}, ErrInvalid
	}
	return s.create(ctx, spec, key)
}

func (s *Scheduler) create(ctx context.Context, spec Spec, key string) (Schedule, error) {
	if spec.Notify != nil {
		if spec.NotifyWeChat != "" {
			return Schedule{}, fmt.Errorf("%w: choose notify or notify_wechat, not both", ErrInvalid)
		}
		for _, v := range []string{spec.Notify.Channel, spec.Notify.Account, spec.Notify.Conversation, spec.Notify.Sender} {
			if v == "" || len(v) > 512 {
				return Schedule{}, fmt.Errorf("%w: invalid channel notification target", ErrInvalid)
			}
		}
	}
	if spec.ID != "" || strings.TrimSpace(spec.Name) == "" || len(spec.Name) > 200 || !utf8.ValidString(spec.Name+spec.Prompt) || !filepath.IsAbs(spec.Workspace) || strings.TrimSpace(spec.Prompt) == "" || len(spec.Prompt) > 16384 || len(spec.NotifyWeChat) > 512 {
		return Schedule{}, fmt.Errorf("%w: name, absolute workspace and prompt are required", ErrInvalid)
	}
	if spec.Approval == "" {
		spec.Approval = "allow_always"
	}
	if spec.Approval != "read_only" && spec.Approval != "ask" && spec.Approval != "allow_always" {
		return Schedule{}, fmt.Errorf("%w: invalid approval mode", ErrInvalid)
	}
	if spec.Timezone == "" {
		spec.Timezone = "Asia/Shanghai"
	}
	canonical := spec
	if spec.Source != nil {
		source := *spec.Source
		source.SessionID = "" // Message identity survives reconnect or a new session.
		canonical.Source = &source
	}
	fingerprint, err := json.Marshal(canonical)
	if err != nil {
		return Schedule{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	if key != "" {
		var id string
		var previous []byte
		err = tx.QueryRowContext(ctx, `SELECT schedule_id,spec FROM harness_schedule_requests WHERE request_key=?`, key).Scan(&id, &previous)
		if err == nil {
			if string(previous) != string(fingerprint) {
				return Schedule{}, fmt.Errorf("%w: request_id already used with different parameters", ErrConflict)
			}
			return scanSchedule(tx.QueryRowContext(ctx, scheduleSelect+` WHERE s.id=?`, id))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, err
		}
	}
	next, err := spec.Next(time.Now())
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if next.IsZero() {
		return Schedule{}, fmt.Errorf("%w: once timestamp must be in the future", ErrInvalid)
	}
	spec.ID = rand.Text()
	body, err := json.Marshal(spec)
	if err != nil {
		return Schedule{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_schedules`).Scan(&count); err != nil {
		return Schedule{}, err
	}
	if count >= 200 {
		return Schedule{}, fmt.Errorf("%w: schedule limit of 200 reached; delete an unused schedule", ErrInvalid)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_schedules(id,spec,paused,next_ms) VALUES(?,?,0,?)`, spec.ID, body, next.UnixMilli())
	if err == nil && key != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_schedule_requests(request_key,schedule_id,spec) VALUES(?,?,?)`, key, spec.ID, fingerprint)
	}
	if err == nil {
		err = tx.Commit()
	}
	return Schedule{Spec: spec, Next: next, Revision: 1, NextLocal: localTime(next, spec.Timezone)}, err
}

func (s *Scheduler) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, scheduleSelect+` ORDER BY s.paused,s.next_ms,s.id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		v, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Scheduler) SetPaused(ctx context.Context, id string, paused bool, revision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := checkedSchedule(ctx, tx, id, revision)
	if err != nil {
		return err
	}
	if paused {
		if v.Paused {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_schedules SET paused=1,pause_reason='manual',revision=revision+1 WHERE id=?`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	if v.Active {
		return ErrBusy
	}
	if !v.Paused {
		return nil
	}
	if v.Spec.Kind == "once" && v.Next.IsZero() {
		return fmt.Errorf("%w: ended once schedules require an explicit future reschedule", ErrInvalid)
	}
	next, err := v.Spec.Next(time.Now())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if next.IsZero() {
		return fmt.Errorf("%w: expired once schedules require a new future time", ErrInvalid)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_schedules SET paused=0,next_ms=?,pause_reason='',revision=revision+1 WHERE id=?`, next.UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Scheduler) Delete(ctx context.Context, id string, revision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := checkedSchedule(ctx, tx, id, revision)
	if err != nil {
		return err
	}
	if v.Active {
		return ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_schedules WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Scheduler) History(ctx context.Context, id string) ([]Occurrence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,schedule_id,due_ms,status,session_id,text,is_read FROM harness_schedule_occurrences WHERE (?='' OR schedule_id=?) ORDER BY due_ms DESC,id DESC LIMIT 100`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Occurrence{}
	for rows.Next() {
		var v Occurrence
		var ms int64
		if err = rows.Scan(&v.ID, &v.ScheduleID, &ms, &v.Outcome.Status, &v.Outcome.SessionID, &v.Outcome.Text, &v.Read); err != nil {
			return nil, err
		}
		v.Due = time.UnixMilli(ms).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Scheduler) Acknowledge(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE harness_schedule_occurrences SET is_read=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Scheduler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if err := s.Tick(ctx, time.Now()); err != nil {
					s.mu.Lock()
					s.lastError = err.Error()
					s.mu.Unlock()
					return
				}
			}
		}
	}()
}

func (s *Scheduler) Status() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := s.done != nil && s.lastError == ""
	if running {
		select {
		case <-s.done:
			running = false
		default:
		}
	}
	return map[string]any{"error": s.lastError, "running": running, "reschedule_version": 1, "notifications": "durable unread occurrences; optional channel delivery"}
}
func (s *Scheduler) Close() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
			<-s.done
		}
	})
}
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Claim and next-fire advancement commit before execution. Missed occurrences
// coalesce to one; interrupted or ambiguous effects are never automatically retried.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var due int64
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id,spec,next_ms FROM harness_schedules WHERE paused=0 AND next_ms>0 AND next_ms<=? ORDER BY next_ms,id LIMIT 1`, now.UnixMilli()).Scan(&id, &raw, &due)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var spec Spec
	if err = json.Unmarshal(raw, &spec); err != nil {
		return err
	}
	var next time.Time
	if spec.Kind != "once" {
		next, err = spec.Next(now)
		if err != nil {
			return err
		}
	}
	nextMS := int64(0)
	if !next.IsZero() {
		nextMS = next.UnixMilli()
	}
	occurrence := rand.Text()
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_schedule_occurrences(id,schedule_id,due_ms,status) VALUES(?,?,?,'running')`, occurrence, id, due); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_schedules SET next_ms=?,paused=?,pause_reason=CASE WHEN ?=0 THEN 'exhausted' ELSE '' END,revision=revision+1 WHERE id=?`, nextMS, nextMS == 0, nextMS, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	out, runErr := s.run(ctx, spec, func(session string) error {
		_, e := s.db.ExecContext(ctx, `UPDATE harness_schedule_occurrences SET session_id=? WHERE id=? AND status='running'`, session, occurrence)
		return e
	})
	if runErr != nil {
		out.Status = "needs_attention"
		out.Text = "Execution stopped; inspect the linked session. " + runErr.Error()
	}
	if out.Status == "" {
		out.Status = "needs_attention"
	}
	if len(out.Text) > 16384 {
		out.Text = string([]rune(out.Text)[:min(len([]rune(out.Text)), 4000)]) + "\n[truncated; inspect session]"
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err = s.db.BeginTx(persist, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if out.Status != "completed" {
		if _, err = tx.ExecContext(persist, `UPDATE harness_schedules SET paused=1,pause_reason='needs_attention' WHERE id=?`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(persist, `UPDATE harness_schedules SET revision=revision+1 WHERE id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(persist, `UPDATE harness_schedule_occurrences SET status=?,session_id=?,text=? WHERE id=?`, out.Status, out.SessionID, out.Text, occurrence); err != nil {
		return fmt.Errorf("persist occurrence: %w", err)
	}
	if (spec.NotifyWeChat != "" || spec.Notify != nil) && s.NotifyTx != nil {
		if err = s.NotifyTx(persist, tx, spec, Occurrence{ID: occurrence, ScheduleID: id, Due: time.UnixMilli(due), Outcome: out}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
