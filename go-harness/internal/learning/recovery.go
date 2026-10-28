package learning

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func (s *Store) extractionRetryReasonTx(ctx context.Context, tx *sql.Tx, session string, epoch int64, job skillmaintenance.Job) (string, error) {
	if job.Request.Kind != skillmaintenance.ExperienceExtract {
		return "unsupported_kind", nil
	}
	if job.State != "failed" {
		return "not_failed", nil
	}
	if !s.enabled || s.maintenance == nil || s.extractor == nil {
		return "host_disabled", nil
	}
	var contribute bool
	var current int64
	err := tx.QueryRowContext(ctx, `SELECT contribute,epoch FROM harness_learning_controls WHERE session_id=?`, session).Scan(&contribute, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return "contribution_disabled", nil
	}
	if err != nil {
		return "", err
	}
	if !contribute {
		return "contribution_disabled", nil
	}
	if current != epoch {
		return "consent_changed", nil
	}
	if job.Attempts >= 3 {
		return "attempts_exhausted", nil
	}
	if !job.Deadline.IsZero() && !job.Deadline.After(s.now().UTC()) {
		return "deadline_expired", nil
	}
	switch job.ErrorCode {
	case "model_failed", "storage_error", "budget_exhausted", "source_active", "worker_interrupted":
	default:
		return "non_retryable_failure", nil
	}
	snap, result, err := s.readExtractionTx(ctx, tx, job)
	if errors.Is(err, skillmaintenance.ErrSourceActive) {
		return "source_active", nil
	}
	if err != nil {
		return "source_unavailable", nil
	}
	if snap.Session != session || snap.Epoch != epoch {
		return "consent_changed", nil
	}
	if result != "" {
		return "result_committed", nil
	}
	cache, err := newDiagnosticCache(ctx, tx)
	if err != nil {
		return "", err
	}
	if cache.backpressure {
		return "queue_backpressure", nil
	}
	available, err := s.maintenance.BudgetAvailableTx(ctx, tx, job)
	if err != nil {
		return "", err
	}
	if !available {
		return "budget_exhausted", nil
	}
	ready, err := s.budgetReadyTx(ctx, tx, job)
	if err != nil {
		return "", err
	}
	if !ready {
		return "budget_exhausted", nil
	}
	return "", nil
}

func (s *Store) RetryExtraction(ctx context.Context, session string, request harness.SkillLearningExtractionRetryRequest) (harness.SkillLearningJob, error) {
	var result harness.SkillLearningJob
	if request.JobID == "" || len(request.JobID) > 128 || request.ExpectedEpoch < 1 || request.ExpectedStateVersion < 1 {
		return result, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var episode string
	var epoch int64
	if err = tx.QueryRowContext(ctx, `SELECT episode_id,epoch FROM df_learning_extractions WHERE session_id=? AND job_id=?`, session, request.JobID).Scan(&episode, &epoch); errors.Is(err, sql.ErrNoRows) {
		return result, harness.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	var current int64
	var contribute bool
	if err = tx.QueryRowContext(ctx, `SELECT epoch,contribute FROM harness_learning_controls WHERE session_id=?`, session).Scan(&current, &contribute); err != nil {
		return result, err
	}
	if !s.enabled || !contribute || s.maintenance == nil {
		return result, harness.ErrPermissionDenied
	}
	if current != request.ExpectedEpoch || epoch != current {
		return result, harness.ErrSkillConflict
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, request.JobID)
	if err != nil {
		return result, err
	}
	replay := job.State == "queued" && job.StateVersion == request.ExpectedStateVersion+1
	if !replay && (job.State != "failed" || job.StateVersion != request.ExpectedStateVersion) {
		return result, harness.ErrSkillConflict
	}
	if replay {
		// Idempotency still validates all source and consent preconditions.
		if _, committed, err := s.readExtractionTx(ctx, tx, job); err != nil {
			return result, err
		} else if committed != "" {
			return result, harness.ErrSkillConflict
		}
	} else {
		reason, err := s.extractionRetryReasonTx(ctx, tx, session, current, job)
		if err != nil {
			return result, err
		}
		if reason != "" {
			return result, harness.ErrSkillConflict
		}
	}
	job, err = s.maintenance.RetryFailedExtractionTx(ctx, tx, job.ID, request.ExpectedStateVersion)
	if err != nil {
		return result, err
	}
	if !replay {
		if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='queued',reason='' WHERE job_id=? AND session_id=? AND epoch=? AND state='skipped'`, job.ID, session, current); err != nil {
			return result, err
		}
	}
	result = harness.SkillLearningJob{ID: job.ID, EpisodeID: episode, Kind: string(job.Request.Kind), State: job.State, StateVersion: job.StateVersion, Attempts: job.Attempts, ErrorCode: job.ErrorCode, Deadline: job.Deadline}
	return result, tx.Commit()
}
