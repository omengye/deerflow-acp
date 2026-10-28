package learning

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func (s *Store) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.closed || s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Metadata-only queries also work before the optional maintenance store exists.
func diagnosticJobsSQL(ctx context.Context, tx *sql.Tx) (string, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&exists)
	if err != nil || !exists {
		return `(SELECT '' id,'' state,'' error_code,'' job_kind,'' owner_kind,0 rowid WHERE 0)`, err
	}
	return `df_skill_maintenance_jobs`, nil
}

type pendingDiagnostic struct {
	run, root, episode, session, status string
	epoch                               int64
	uncertain                           bool
}

type diagnosticCache struct {
	roots        map[string]bool
	episodes     map[string]string
	queued       map[string]string
	jobs         string
	backpressure bool
}

func newDiagnosticCache(ctx context.Context, tx *sql.Tx) (*diagnosticCache, error) {
	jobs, err := diagnosticJobsSQL(ctx, tx)
	if err != nil {
		return nil, err
	}
	c := &diagnosticCache{roots: map[string]bool{}, episodes: map[string]string{}, queued: map[string]string{}, jobs: jobs}
	err = tx.QueryRowContext(ctx, `SELECT count(*)>=100 OR COALESCE(sum(CASE WHEN owner_kind<>'skill' THEN 1 ELSE 0 END),0)>=20 FROM `+jobs+` WHERE state IN ('queued','running','retry')`).Scan(&c.backpressure)
	return c, err
}

func (s *Store) queuedReasonTx(ctx context.Context, tx *sql.Tx, id string, cache *diagnosticCache) (string, error) {
	if cache.jobs != "df_skill_maintenance_jobs" {
		return "job_missing", nil
	}
	if reason, found := cache.queued[id]; found {
		return reason, nil
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, id)
	if errors.Is(err, harness.ErrNotFound) {
		return "job_missing", nil
	}
	if err != nil {
		return "", err
	}
	reason := "awaiting_cleanup"
	switch job.State {
	case "running":
		reason = "extracting"
	case "retry":
		reason = "retry_backoff"
	case "failed":
		reason = job.ErrorCode
		if reason == "" {
			reason = "job_failed"
		}
	case "cancelled":
		reason = "cancelled"
	case "queued":
		switch {
		case !s.enabled:
			reason = "host_disabled"
		case job.Attempts >= 3:
			reason = "attempts_exhausted"
		case !job.Deadline.IsZero() && !job.Deadline.After(s.now().UTC()):
			reason = "deadline_expired"
		default:
			if s.maintenance != nil {
				available, err := s.maintenance.BudgetAvailableTx(ctx, tx, job)
				if err != nil {
					return "", err
				}
				if !available {
					reason = "budget_exhausted"
					break
				}
			}
			budget, err := s.budgetReadyTx(ctx, tx, job)
			if err != nil {
				return "", err
			}
			if !budget {
				reason = "budget_exhausted"
				break
			}
			ready, err := s.ReadyTx(ctx, tx, job)
			if err != nil {
				return "", err
			}
			if ready {
				reason = "awaiting_worker"
			} else {
				reason = "source_active"
			}
		}
	}
	cache.queued[id] = reason
	return reason, nil
}

// Match QueueOne's live gates, not the last persisted reason. An old empty or
// tree_unsettled reason must not hide recovery or report a ready input as stuck.
func (s *Store) pendingReasonTx(ctx context.Context, tx *sql.Tx, p pendingDiagnostic, c *diagnosticCache) (string, error) {
	if !s.enabled {
		return "host_disabled", nil
	}
	var contribute bool
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT contribute,epoch FROM harness_learning_controls WHERE session_id=?`, p.session).Scan(&contribute, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "contribution_disabled", nil
	}
	if err != nil {
		return "", err
	}
	if !contribute {
		return "contribution_disabled", nil
	}
	if epoch != p.epoch {
		return "consent_changed", nil
	}
	if p.uncertain || p.status == "cancelled" {
		return "unsafe_outcome", nil
	}
	settled, found := c.roots[p.root]
	if !found {
		settled, err = settledRoot(ctx, tx, p.root)
		if err != nil {
			return "", err
		}
		c.roots[p.root] = settled
	}
	if !settled {
		return "tree_unsettled", nil
	}
	key := encodedDigest([]any{p.episode, p.epoch})
	if reason, found := c.episodes[key]; found {
		return reason, nil
	}
	active, err := episodeTailActiveTx(ctx, tx, p.episode, p.epoch)
	if err != nil {
		return "", err
	}
	reason := "ready"
	if active {
		reason = "source_active"
	} else {
		var idle, extracting bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_learning_outbox WHERE episode_id=? AND julianday(ended_at)>julianday(?)), EXISTS(SELECT 1 FROM df_learning_extractions x JOIN `+c.jobs+` j ON j.id=x.job_id WHERE x.episode_id=? AND j.state IN ('queued','retry','running'))`, p.episode, s.now().UTC().Add(-s.options.Idle).Format(time.RFC3339Nano), p.episode).Scan(&idle, &extracting)
		if err != nil {
			return "", err
		}
		switch {
		case idle:
			reason = "idle_window"
		case extracting:
			reason = "extraction_active"
		case c.backpressure:
			reason = "queue_backpressure"
		}
	}
	c.episodes[key] = reason
	return reason, nil
}

func (s *Store) queueStatusTx(ctx context.Context, tx *sql.Tx, session string, cache *diagnosticCache) (harness.SkillLearningQueueStatus, error) {
	q := harness.SkillLearningQueueStatus{Reasons: []harness.SkillLearningQueueReason{}}
	reasons := map[string]int64{}
	queued := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT o.state,o.reason,o.job_id,COALESCE(j.state,''),COALESCE(j.error_code,''),count(*) FROM harness_learning_outbox o LEFT JOIN `+cache.jobs+` j ON j.id=o.job_id WHERE (?='' OR o.session_id=?) GROUP BY o.state,o.reason,o.job_id,j.state,j.error_code`, session, session)
	if err != nil {
		return q, err
	}
	for rows.Next() {
		var state, reason, jobID, jobState, code string
		var count int64
		if err = rows.Scan(&state, &reason, &jobID, &jobState, &code, &count); err != nil {
			break
		}
		switch state {
		case "pending":
			q.Pending += count
		case "queued":
			q.Queued += count
			if jobState == "" {
				reasons["job_missing"] += count
			} else {
				queued[jobID] += count
			}
		case "skipped":
			if jobState == "failed" {
				q.Failed += count
				if code != "" {
					reason = code
				}
			} else {
				q.Skipped += count
			}
			if reason != "" {
				reasons[reason] += count
			}
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return q, err
	}
	for id, count := range queued {
		reason, err := s.queuedReasonTx(ctx, tx, id, cache)
		if err != nil {
			return q, err
		}
		reasons[reason] += count
	}
	rows, err = tx.QueryContext(ctx, `SELECT o.run_id,i.root_id,o.episode_id,o.session_id,o.epoch,o.status,o.effects_uncertain FROM harness_learning_outbox o JOIN harness_learning_inputs i ON i.run_id=o.run_id WHERE o.state='pending' AND (?='' OR o.session_id=?)`, session, session)
	if err != nil {
		return q, err
	}
	var pending []pendingDiagnostic
	for rows.Next() {
		var p pendingDiagnostic
		if err = rows.Scan(&p.run, &p.root, &p.episode, &p.session, &p.epoch, &p.status, &p.uncertain); err != nil {
			break
		}
		pending = append(pending, p)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return q, err
	}
	for _, p := range pending {
		reason, err := s.pendingReasonTx(ctx, tx, p, cache)
		if err != nil {
			return q, err
		}
		if reason == "ready" {
			q.Ready++
		} else {
			q.Blocked++
			reasons[reason]++
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+cache.jobs+` j WHERE j.state='failed' AND j.job_kind IN ('experience_extract','experience_consolidate','skill_match') AND (?='' OR j.id IN (SELECT job_id FROM df_learning_extractions WHERE session_id=? UNION SELECT job_id FROM df_learning_consolidations WHERE session_id=? UNION SELECT job_id FROM df_learning_matches WHERE session_id=?))`, session, session, session, session).Scan(&q.FailedJobs)
	if err != nil {
		return q, err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT i.session_id FROM harness_learning_inputs i JOIN harness_runs r ON r.id=i.run_id WHERE (?='' OR i.session_id=?) AND NOT EXISTS(SELECT 1 FROM harness_learning_outbox o WHERE o.run_id=i.run_id AND o.state NOT IN ('pending','queued')) GROUP BY i.session_id HAVING count(*)>=128)`, session, session).Scan(&q.CaptureLimitedSessions)
	if err != nil {
		return q, err
	}
	for code, count := range reasons {
		q.Reasons = append(q.Reasons, harness.SkillLearningQueueReason{Code: code, Count: count})
	}
	sort.Slice(q.Reasons, func(i, j int) bool { return q.Reasons[i].Code < q.Reasons[j].Code })
	if len(q.Reasons) > 64 {
		var other int64
		for _, reason := range q.Reasons[63:] {
			other += reason.Count
		}
		q.Reasons = append(q.Reasons[:63], harness.SkillLearningQueueReason{Code: "other", Count: other})
	}
	return q, nil
}

func (s *Store) Diagnostics(ctx context.Context, session string, limit int) (harness.SkillLearningDiagnostics, error) {
	d := harness.SkillLearningDiagnostics{SessionID: session, Running: s.running(), Inputs: []harness.SkillLearningDiagnosticInput{}, Jobs: []harness.SkillLearningDiagnosticJob{}}
	if limit < 1 || limit > 100 {
		return d, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(c.epoch,0),COALESCE(c.contribute,?) FROM harness_sessions session LEFT JOIN harness_learning_controls c ON c.session_id=session.id WHERE session.id=?`, s.enabled, session).Scan(&d.ConsentEpoch, &d.Contribute)
	if errors.Is(err, sql.ErrNoRows) {
		return d, harness.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	cache, err := newDiagnosticCache(ctx, tx)
	if err != nil {
		return d, err
	}
	if d.Queue, err = s.queueStatusTx(ctx, tx, session, cache); err != nil {
		return d, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.run_id,i.root_id,i.episode_id,COALESCE(o.state,'awaiting_completion'),COALESCE(o.reason,''),COALESCE(o.ended_at,''),COALESCE(o.job_id,''),r.status,i.epoch,COALESCE(o.effects_uncertain,0) FROM harness_learning_inputs i JOIN harness_runs r ON r.id=i.run_id LEFT JOIN harness_learning_outbox o ON o.run_id=i.run_id WHERE i.session_id=? ORDER BY CASE WHEN o.state IS NULL THEN 0 WHEN o.state='pending' THEN 1 WHEN o.state='queued' THEN 2 WHEN o.state='skipped' THEN 3 ELSE 4 END,i.accepted_sequence DESC LIMIT ?`, session, limit+1)
	if err != nil {
		return d, err
	}
	var pending []pendingDiagnostic
	for rows.Next() {
		var in harness.SkillLearningDiagnosticInput
		var p pendingDiagnostic
		if err = rows.Scan(&in.RunID, &in.RootID, &in.EpisodeID, &in.State, &in.Reason, &in.EndedAt, &in.JobID, &in.RunStatus, &p.epoch, &p.uncertain); err != nil {
			break
		}
		if len(d.Inputs) == limit {
			d.Truncated = true
			break
		}
		p.run, p.root, p.episode, p.session, p.status = in.RunID, in.RootID, in.EpisodeID, session, in.RunStatus
		d.Inputs = append(d.Inputs, in)
		pending = append(pending, p)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return d, err
	}
	for i := range d.Inputs {
		in := &d.Inputs[i]
		if in.State == "pending" {
			in.Reason, err = s.pendingReasonTx(ctx, tx, pending[i], cache)
		}
		if in.State == "queued" {
			in.Reason, err = s.queuedReasonTx(ctx, tx, in.JobID, cache)
		}
		if err != nil {
			return d, err
		}
		if in.State == "awaiting_completion" {
			in.Reason = "source_active"
			if in.RunStatus == "needs_reconciliation" || in.RunStatus == "waiting_input" {
				in.Reason = in.RunStatus
			}
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,episode_id,pattern_id,revision FROM (
 SELECT j.id,x.episode_id,'' pattern_id,0 revision,j.state,j.rowid ordered FROM df_learning_extractions x JOIN `+cache.jobs+` j ON j.id=x.job_id WHERE x.session_id=?
 UNION ALL SELECT j.id,e.episode_id,'',0,j.state,j.rowid FROM df_learning_consolidations c JOIN df_learning_experiences e ON e.id=c.experience_id JOIN `+cache.jobs+` j ON j.id=c.job_id WHERE c.session_id=?
 UNION ALL SELECT j.id,'',m.pattern_id,m.revision,j.state,j.rowid FROM df_learning_matches m JOIN `+cache.jobs+` j ON j.id=m.job_id WHERE m.session_id=? AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_sharing sh WHERE sh.pattern_id=m.pattern_id))
 ORDER BY CASE state WHEN 'failed' THEN 0 WHEN 'running' THEN 1 WHEN 'queued' THEN 2 WHEN 'retry' THEN 3 ELSE 4 END,ordered DESC LIMIT ?`, session, session, session, limit+1)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var j harness.SkillLearningDiagnosticJob
		if err = rows.Scan(&j.ID, &j.EpisodeID, &j.PatternID, &j.PatternRevision); err != nil {
			break
		}
		if len(d.Jobs) == limit {
			d.Truncated = true
			break
		}
		d.Jobs = append(d.Jobs, j)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return d, err
	}
	for i := range d.Jobs {
		j, err := skillmaintenance.ReadJob(ctx, tx, d.Jobs[i].ID)
		if err != nil {
			return d, err
		}
		out := &d.Jobs[i]
		out.Kind, out.State, out.StateVersion, out.Attempts, out.ErrorCode, out.Deadline = string(j.Request.Kind), j.State, j.StateVersion, j.Attempts, j.ErrorCode, j.Deadline
		out.RetryReason, err = s.extractionRetryReasonTx(ctx, tx, session, d.ConsentEpoch, j)
		if err != nil {
			return d, err
		}
		out.Retryable = out.RetryReason == ""
	}
	return d, tx.Commit()
}
