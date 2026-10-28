package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
)

// BudgetAvailableTx is a read-only scheduling hint for aggregate exhaustion.
// It includes every maintenance producer and both pinned and current policy.
// A positive result does not promise that a particular model call fits: only
// Reserve knows its input/output estimate and can authorize provider I/O.
func (s *Store) BudgetAvailableTx(ctx context.Context, tx *sql.Tx, job Job) (bool, error) {
	var calls, tokens int64
	err := tx.QueryRowContext(ctx, `SELECT model_calls,charged_tokens FROM df_skill_maintenance_days WHERE day=?`, s.now().UTC().Format("2006-01-02")).Scan(&calls, &tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return calls < min(job.Policy.MaxDailyModelCalls, s.policy.MaxDailyModelCalls) && tokens < min(job.Policy.MaxDailyTokens, s.policy.MaxDailyTokens), nil
}

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
	available, err := s.BudgetAvailableTx(ctx, tx, j)
	if err != nil {
		return j, err
	}
	if !available {
		return j, ErrBudget
	}
	var pending, learning int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN owner_kind<>'skill' THEN 1 ELSE 0 END),0) FROM df_skill_maintenance_jobs WHERE state IN ('queued','running','retry')`).Scan(&pending, &learning); err != nil {
		return j, err
	}
	if pending >= 100 || learning >= 20 {
		return j, ErrBudget
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
