package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func migrateSessionRetention(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(harness_sessions)`)
	if err != nil {
		return err
	}
	retentionColumn := false
	for rows.Next() {
		var ordinal, required, primary int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&ordinal, &name, &kind, &required, &defaultValue, &primary); err != nil {
			break
		}
		if name == "closed_at" {
			retentionColumn = true
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if !retentionColumn {
		if _, err = db.ExecContext(ctx, `ALTER TABLE harness_sessions ADD COLUMN closed_at TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS harness_sessions_retention ON harness_sessions(closed_at,updated_at)`)
	return err
}

func (s *Store) MarkClosed(ctx context.Context, id string) error {
	now := timestamp()
	result, err := s.db.ExecContext(ctx, `UPDATE harness_sessions SET closed_at=?,updated_at=? WHERE id=?`, now, now, id)
	return sessionUpdateResult(result, err)
}

func (s *Store) MarkOpen(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE harness_sessions SET closed_at='',updated_at=? WHERE id=?`, timestamp(), id)
	return sessionUpdateResult(result, err)
}

func sessionUpdateResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return harness.ErrNotFound
	}
	return nil
}

func ValidateRetention(policy harness.RetentionPolicy) error {
	if policy.ClosedDays < 0 || policy.ClosedDays > 3650 || policy.InactiveDays < 0 || policy.InactiveDays > 3650 || policy.CheckInterval < 0 || policy.CheckInterval > 24*time.Hour {
		return harness.ErrInvalidInput
	}
	if !policy.Enabled {
		return nil
	}
	if policy.InactiveDays < 1 || policy.CheckInterval < time.Minute {
		return harness.ErrInvalidInput
	}
	return nil
}

// ExpiredSessionIDsPage scans a bounded keyset of foreground sessions. The
// cursor advances over every inspected ID, including blocked or unexpired
// sessions, so old blocked candidates cannot starve later cleanup work.
func (s *Store) ExpiredSessionIDsPage(ctx context.Context, now time.Time, policy harness.RetentionPolicy, after string, limit int) ([]string, string, error) {
	if !policy.Enabled || limit < 1 || limit > 1000 {
		return nil, "", harness.ErrInvalidInput
	}
	if err := ValidateRetention(policy); err != nil {
		return nil, "", err
	}
	children, err := s.hasBackgroundSessions(ctx)
	if err != nil {
		return nil, "", err
	}
	childFilter := ""
	if children {
		childFilter = ` AND NOT EXISTS(SELECT 1 FROM harness_background_specs b WHERE b.child_session_id=s.id)`
	}
	query := `SELECT s.id,s.closed_at,s.updated_at FROM harness_sessions s WHERE s.id>?` + childFilter + ` ORDER BY s.id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, after, limit)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, limit)
	last := ""
	seen := 0
	for rows.Next() {
		var id, closed, updated string
		if err = rows.Scan(&id, &closed, &updated); err != nil {
			break
		}
		seen++
		last = id
		raw, days := updated, policy.InactiveDays
		if closed != "" {
			raw, days = closed, policy.ClosedDays
		}
		var when time.Time
		when, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			break
		}
		if !when.After(now.UTC().AddDate(0, 0, -days)) {
			ids = append(ids, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	if seen < limit {
		last = ""
	}
	return ids, last, nil
}

// IsExpiredSession rechecks a candidate after a cleanup reservation excludes
// foreground reconnects. A missing row is harmless after a prior deletion.
func (s *Store) IsExpiredSession(ctx context.Context, id string, now time.Time, policy harness.RetentionPolicy) (bool, error) {
	var closed, updated string
	err := s.db.QueryRowContext(ctx, `SELECT closed_at,updated_at FROM harness_sessions WHERE id=?`, id).Scan(&closed, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, days := updated, policy.InactiveDays
	if closed != "" {
		raw, days = closed, policy.ClosedDays
	}
	when, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false, err
	}
	return !when.After(now.UTC().AddDate(0, 0, -days)), nil
}

// IsCleanupEligible is an inventory hint. The actual purge repeats all checks
// under a deletion reservation, since task bindings and receipts may change.
func (s *Store) IsCleanupEligible(ctx context.Context, id string, now time.Time, policy harness.RetentionPolicy) (bool, error) {
	expired, err := s.IsExpiredSession(ctx, id, now, policy)
	if err != nil || !expired {
		return false, err
	}
	var unresolved bool
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('pending','started','uncertain'))`, id).Scan(&unresolved); err != nil || unresolved {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	tables, err := sessionTables(ctx, tx)
	if err != nil {
		return false, err
	}
	_, _, err = prepareBackgroundGraphTx(ctx, tx, id, tables, true)
	if errors.Is(err, harness.ErrBusy) || errors.Is(err, harness.ErrInvalidInput) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
