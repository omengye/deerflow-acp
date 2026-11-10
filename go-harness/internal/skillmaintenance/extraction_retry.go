package skillmaintenance

import (
	"context"
	"database/sql"
)

// RetryFailedExtractionTx reuses a failed extraction's remaining lifetime.
// The producer must validate its immutable snapshot and consent in this same
// transaction first. Attempts, deadline, policies and all charged receipts are
// deliberately preserved; this cannot create a fresh budget or replay tools.
func (s *Store) RetryFailedExtractionTx(ctx context.Context, tx *sql.Tx, id string, version int64) (Job, error) {
	j, err := ReadJob(ctx, tx, id)
	if err != nil {
		return j, err
	}
	if version < 1 || j.Request.Kind != ExperienceExtract {
		return j, ErrInvalid
	}
	if j.State == "queued" && j.StateVersion == version+1 {
		return j, nil
	}
	if j.State != "failed" || j.StateVersion != version || j.Attempts >= maxAttempts {
		return j, ErrConflict
	}
	now, err := s.clock(ctx, tx)
	if err != nil {
		return j, err
	}
	if !j.Deadline.IsZero() && !j.Deadline.After(now) {
		return j, ErrConflict
	}
	var pending, learning int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN owner_kind<>'skill' THEN 1 ELSE 0 END),0) FROM df_skill_maintenance_jobs WHERE state IN ('queued','running','retry')`).Scan(&pending, &learning); err != nil {
		return j, err
	}
	if pending >= 100 || learning >= 20 {
		return j, ErrCapacity
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='queued',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='' WHERE id=? AND state='failed' AND state_version=?`, id, version)
	if err != nil {
		return j, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return j, err
	}
	if n != 1 {
		return j, ErrConflict
	}
	return ReadJob(ctx, tx, id)
}
