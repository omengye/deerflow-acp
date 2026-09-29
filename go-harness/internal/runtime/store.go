package runtime

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type Store struct{ db *sql.DB }

func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_sessions (id TEXT PRIMARY KEY, cwd TEXT NOT NULL, title TEXT NOT NULL, mode TEXT NOT NULL, model TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS harness_runs (id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES harness_sessions(id), input_id TEXT UNIQUE NOT NULL, status TEXT NOT NULL, stop_reason TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS harness_inputs (id TEXT PRIMARY KEY, run_id TEXT UNIQUE NOT NULL REFERENCES harness_runs(id), content BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES harness_sessions(id), run_id TEXT NOT NULL, event BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS harness_events_session ON harness_events(session_id,sequence);
CREATE TABLE IF NOT EXISTS harness_approvals (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, run_id TEXT NOT NULL, intent BLOB NOT NULL, decision TEXT NOT NULL, updated_at TEXT NOT NULL);
`)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func NewID() string     { return rand.Text() }
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) CreateSession(ctx context.Context, cwd, model string) (harness.Session, error) {
	x := harness.Session{ID: NewID(), CWD: cwd, Mode: "default", Model: model, CreatedAt: time.Now().UTC()}
	x.UpdatedAt = x.CreatedAt
	_, err := s.db.ExecContext(ctx, `INSERT INTO harness_sessions VALUES(?,?,?,?,?,?,?)`, x.ID, x.CWD, x.Title, x.Mode, x.Model, x.CreatedAt.Format(time.RFC3339Nano), x.UpdatedAt.Format(time.RFC3339Nano))
	return x, err
}

type scanner interface{ Scan(...any) error }

func scanSession(row scanner) (harness.Session, error) {
	var x harness.Session
	var created, updated string
	err := row.Scan(&x.ID, &x.CWD, &x.Title, &x.Mode, &x.Model, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return x, harness.ErrNotFound
	}
	if err != nil {
		return x, err
	}
	x.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return x, err
	}
	x.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return x, err
}
func (s *Store) Session(ctx context.Context, id string) (harness.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT id,cwd,title,mode,model,created_at,updated_at FROM harness_sessions WHERE id=?`, id))
}
func (s *Store) List(ctx context.Context, cwd, cursor string, limit int) ([]harness.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,cwd,title,mode,model,created_at,updated_at FROM harness_sessions WHERE (?='' OR cwd=?) AND id>? ORDER BY id LIMIT ?`, cwd, cwd, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]harness.Session, 0)
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, x)
	}
	return result, rows.Err()
}
func (s *Store) SetMode(ctx context.Context, id, mode string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE harness_sessions SET mode=?,updated_at=? WHERE id=?`, mode, timestamp(), id)
	return err
}

// BeginRun atomically records accepted input and its replay event before the
// model sees it. Recovery can distinguish accepted input from a finished turn.
func (s *Store) BeginRun(ctx context.Context, req harness.RunRequest) error {
	input, err := json.Marshal(req.Input)
	if err != nil {
		return err
	}
	user, err := json.Marshal(harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: "user_message", Content: req.Input})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'running',?,?)`, req.RunID, req.Session.ID, req.InputID, now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_inputs VALUES(?,?,?)`, req.InputID, req.RunID, input); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, req.Session.ID, req.RunID, user); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, now, req.Session.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Append(ctx context.Context, e harness.RunEvent) (harness.RunEvent, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, e.SessionID, e.RunID, data)
	if err != nil {
		return e, err
	}
	e.Sequence, err = r.LastInsertId()
	return e, err
}
func (s *Store) History(ctx context.Context, id string) ([]harness.RunEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,event FROM harness_events WHERE session_id=? ORDER BY sequence`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []harness.RunEvent
	for rows.Next() {
		var seq int64
		var data []byte
		if err = rows.Scan(&seq, &data); err != nil {
			return nil, err
		}
		var e harness.RunEvent
		if err = json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		e.Sequence = seq
		events = append(events, e)
	}
	return events, rows.Err()
}
func (s *Store) Finish(ctx context.Context, id, reason string, runErr error) error {
	status := "completed"
	detail := ""
	if reason == "cancelled" {
		status = "cancelled"
	}
	if runErr != nil {
		status = "failed"
		detail = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE harness_runs SET status=?,stop_reason=?,error=?,updated_at=? WHERE id=?`, status, reason, detail, timestamp(), id)
	return err
}
func (s *Store) Approval(ctx context.Context, p harness.PermissionRequest) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO harness_approvals VALUES(?,?,?,?,?,?)`, p.ID, p.SessionID, p.RunID, data, "pending", timestamp())
	return err
}
func (s *Store) Decide(ctx context.Context, id string, d harness.PermissionDecision) error {
	_, err := s.db.ExecContext(ctx, `UPDATE harness_approvals SET decision=?,updated_at=? WHERE id=?`, d, timestamp(), id)
	return err
}

// ReconcileInterrupted never blindly replays model turns or external effects.
// It preserves the accepted input for later explicit reconciliation.
func (s *Store) ReconcileInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE harness_runs SET status='needs_reconciliation',error='process stopped before final run receipt',updated_at=? WHERE status='running'`, timestamp())
	if err != nil {
		return fmt.Errorf("reconcile runs: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE harness_approvals SET decision='cancelled',updated_at=? WHERE decision='pending'`, timestamp())
	return err
}
