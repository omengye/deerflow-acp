package skillmaintenance

import (
	"context"
	"database/sql"
)

// ObserveDependency records a failed preflight without waking waiting jobs.
// The next healthy identity can then wake them even after a transient outage.
func (s *Store) ObserveDependency(ctx context.Context, code, identity string) error {
	if code != "blocked_runtime" && code != "deferred_capacity" {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO df_skill_maintenance_dependencies VALUES(?,?) ON CONFLICT(code) DO UPDATE SET identity=excluded.identity`, code, identity)
	return err
}

// MarkProgressTx is called only after the stage has persisted an immutable
// result. Unique keys make replay unable to renew a slice indefinitely.
func MarkProgressTx(ctx context.Context, tx *sql.Tx, job, key string) error {
	if err := GuardTx(ctx, tx, job); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_maintenance_progress(job_id,progress_key) VALUES(?,?)`, job, key)
	return err
}

func (s *Store) WakeChanged(ctx context.Context, code, identity string) error {
	if code != "blocked_runtime" && code != "blocked_evidence" && code != "deferred_capacity" {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT identity FROM df_skill_maintenance_dependencies WHERE code=?`, code).Scan(&prior)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if prior == identity {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_maintenance_dependencies VALUES(?,?) ON CONFLICT(code) DO UPDATE SET identity=excluded.identity`, code, identity); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='queued',state_version=state_version+1,attempts=0,deadline_ns=0,retry_ns=0 WHERE state='waiting' AND error_code=?`, code); err != nil {
		return err
	}
	return tx.Commit()
}
