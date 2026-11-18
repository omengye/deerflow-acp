package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type flushItem struct{ input, run, job string }
type FlushSet struct {
	session, barrier string
	items            []flushItem
}

// CaptureFlush is called while the foreground slot is owned. No transaction or
// slot is held by the later wait. Legacy jobs without input decisions remain in
// the set so an empty queue cannot conceal a historical failure.
func (s *Store) CaptureFlush(ctx context.Context, session string) (set FlushSet, err error) {
	set.session = session
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT input_id,run_id,job_id FROM memory_inputs WHERE session_id=?
 UNION ALL SELECT '',j.run_id,j.id FROM memory_jobs j LEFT JOIN harness_runs r ON r.id=j.run_id
 WHERE (r.session_id=? OR (CASE WHEN json_valid(CAST(j.payload AS TEXT)) THEN json_extract(CAST(j.payload AS TEXT),'$.session_id') ELSE '' END)=?)
 AND NOT EXISTS(SELECT 1 FROM memory_inputs i WHERE i.job_id=j.id) ORDER BY 1,2,3`, session, session, session)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var x flushItem
			if e = rows.Scan(&x.input, &x.run, &x.job); e != nil {
				return e
			}
			set.items = append(set.items, x)
		}
		return rows.Err()
	})
	if err != nil {
		return set, fmt.Errorf("%w: %w", harness.ErrMemoryFlushState, err)
	}
	parts := []string{"memory-flush-v1", session}
	for _, x := range set.items {
		parts = append(parts, x.input, x.run, x.job)
	}
	set.barrier = OperationID(parts...)
	return set, nil
}

// Worker state is process-local; stopped workers do not imply completed jobs.
func (s *Store) SetExtractionWorkerRunning(running bool) { s.extractionRunning.Store(running) }
func (s *Store) ExtractionWorkerRunning() bool           { return s.extractionRunning.Load() }

func (s *Store) ObserveFlush(ctx context.Context, set FlushSet) (status harness.MemoryFlushStatus, err error) {
	status.Barrier, status.Total = set.barrier, len(set.items)
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		var sessionExists bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)", set.session).Scan(&sessionExists); e != nil {
			return e
		}
		for _, x := range set.items {
			decision := ""
			job := x.job
			if x.input != "" {
				var run, session string
				if e := tx.QueryRowContext(ctx, "SELECT decision,job_id,run_id,session_id FROM memory_inputs WHERE input_id=?", x.input).Scan(&decision, &job, &run, &session); e != nil {
					return e
				}
				if job != x.job || run != x.run || session != set.session {
					return harness.ErrMemoryFlushState
				}
			}
			var state, code, available, jobRun, jobInput string
			if job != "" {
				if e := tx.QueryRowContext(ctx, "SELECT status,error_code,available_at,run_id,input_id FROM memory_jobs WHERE id=?", job).Scan(&state, &code, &available, &jobRun, &jobInput); e != nil {
					return e
				}
				if jobRun != x.run || (x.input != "" && jobInput != x.input) {
					return harness.ErrMemoryFlushState
				}
			}
			// Durable terminal outcomes survive source deletion and are not reinterpreted.
			switch state {
			case "completed":
				if skippedDecision(decision) || code == "quota_skip" || code == "explicit_action" {
					status.Skipped++
				} else {
					status.Completed++
				}
				continue
			case "failed":
				status.Failed++
				setFlushReason(&status, "failed")
				continue
			case "discarded", "cancelled":
				status.Discarded++
				continue
			}
			if job == "" {
				if skippedDecision(decision) {
					status.Skipped++
					continue
				}
				if decision == "processed" || decision == "promoted" || decision == "partial" {
					status.Completed++
					continue
				}
				if decision == "discarded" {
					status.Discarded++
					continue
				}
			}
			if !sessionExists && decision != "discarded" {
				status.Blocked++
				setFlushReason(&status, "orphan_source")
				continue
			}
			var runState, runSession string
			e := tx.QueryRowContext(ctx, "SELECT status,session_id FROM harness_runs WHERE id=?", x.run).Scan(&runState, &runSession)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if errors.Is(e, sql.ErrNoRows) {
				if decision == "discarded" {
					status.Discarded++
				} else {
					status.Blocked++
					setFlushReason(&status, "orphan_source")
				}
				continue
			}
			if runSession != set.session {
				return harness.ErrMemoryFlushState
			}
			if runState == "failed" || runState == "cancelled" {
				// Only queued jobs have no active call. In-flight accounting is handled by
				// recovery; a dispatched unknown remains blocked with its hold intact.
				if state == "queued" {
					if e = discardQueuedSourceTx(ctx, tx, job, x.input, "run_not_completed"); e != nil {
						return e
					}
				} else if state == "running" || state == "blocked" {
					status.Blocked++
					setFlushReason(&status, "budget_unknown")
					continue
				} else if job == "" {
					if _, e = tx.ExecContext(ctx, "UPDATE memory_inputs SET decision='discarded' WHERE input_id=?", x.input); e != nil {
						return e
					}
				}
				status.Discarded++
				continue
			}
			if state == "blocked" {
				status.Blocked++
				setFlushReason(&status, "budget_unknown")
				continue
			}
			if runState != "completed" {
				status.Blocked++
				setFlushReason(&status, "run_unresolved")
				continue
			}
			switch state {
			case "queued", "running":
				status.Pending++
				if code == "daily_limit" || code == "budget_held" {
					setFlushReason(&status, code)
				} else if next, e := time.Parse(time.RFC3339Nano, available); e == nil && next.After(time.Now()) {
					setFlushReason(&status, "retry_delay")
				} else {
					setFlushReason(&status, "pending")
				}
				if next, e := time.Parse(time.RFC3339Nano, available); e == nil && next.After(time.Now()) {
					previous, _ := time.Parse(time.RFC3339Nano, status.NextAt)
					if previous.IsZero() || next.Before(previous) {
						status.NextAt = available
					}
				}
			case "":
				if skippedDecision(decision) {
					status.Skipped++
				} else if decision == "processed" || decision == "promoted" || decision == "partial" {
					status.Completed++
				} else if decision == "discarded" {
					status.Discarded++
				} else {
					status.Failed++
					setFlushReason(&status, "decision_failed")
				}
			default:
				return harness.ErrMemoryFlushState
			}
		}
		return nil
	})
	if err != nil {
		status.Flushed = false
		status.Reason = "state_error"
		return status, fmt.Errorf("%w: %w", harness.ErrMemoryFlushState, err)
	}
	return status, nil
}

func skippedDecision(d string) bool {
	switch d {
	case "postrun_skipped", "quota_skip", "disabled", "source_text_ineligible", "explicit_action", "no_facts", "superseded", "requires_explicit", "scope_changed", "legacy_source_snapshot_missing", "legacy_memory_policy_unverifiable", "source_unavailable":
		return true
	}
	return false
}
func setFlushReason(s *harness.MemoryFlushStatus, reason string) {
	rank := func(r string) int {
		switch r {
		case "budget_unknown":
			return 10
		case "orphan_source":
			return 9
		case "run_unresolved":
			return 8
		case "failed", "decision_failed":
			return 7
		case "daily_limit":
			return 6
		case "budget_held":
			return 5
		case "retry_delay":
			return 4
		case "pending":
			return 1
		}
		return 0
	}
	if rank(reason) > rank(s.Reason) {
		s.Reason = reason
	}
}
func discardQueuedSourceTx(ctx context.Context, tx *sql.Tx, job, input, reason string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_job_dispositions(job_id,reason,usage_state) VALUES(?,?,'no_active_call') ON CONFLICT(job_id) DO NOTHING", job, reason); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memory_jobs SET status='discarded',error_code=? WHERE id=? AND status='queued'", reason, job); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE memory_inputs SET decision='discarded' WHERE input_id=?", input)
	return err
}

func discardFailedSourcesTx(ctx context.Context, tx *sql.Tx) error {
	// Import/migration tools can open the memory schema without runtime tables.
	// After legacy disposal there may be no remaining source to reconcile.
	var queued bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_jobs WHERE status='queued')").Scan(&queued); err != nil || !queued {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT j.id,j.input_id FROM memory_jobs j JOIN harness_runs r ON r.id=j.run_id WHERE j.status='queued' AND r.status IN ('failed','cancelled')`)
	if err != nil {
		return err
	}
	var items []flushItem
	for rows.Next() {
		var x flushItem
		if err = rows.Scan(&x.job, &x.input); err != nil {
			break
		}
		items = append(items, x)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range items {
		if err = discardQueuedSourceTx(ctx, tx, x.job, x.input, "run_not_completed"); err != nil {
			return err
		}
	}
	return nil
}

// Called inside the session purge transaction after budget deletion eligibility
// checks have excluded active/unknown calls. Keep dispositions after run removal.
func DiscardSessionJobsTx(ctx context.Context, tx *sql.Tx, session string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='memory_job_dispositions')").Scan(&exists); err != nil || !exists {
		return err
	}
	ids := `SELECT j.id FROM memory_jobs j LEFT JOIN harness_runs r ON r.id=j.run_id LEFT JOIN memory_inputs i ON i.job_id=j.id WHERE (r.session_id=? OR i.session_id=?) AND j.status IN ('queued','running','blocked')`
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_job_dispositions(job_id,reason,usage_state) SELECT id,'session_deleted','no_active_call' FROM memory_jobs WHERE id IN (`+ids+`) ON CONFLICT(job_id) DO NOTHING`, session, session); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_job_leases SET lease_id='',lease_ns=0 WHERE job_id IN (`+ids+`)`, session, session); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='discarded',error_code='session_deleted' WHERE id IN (`+ids+`)`, session, session); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='discarded' WHERE session_id=? AND (job_id IN(SELECT job_id FROM memory_job_dispositions WHERE reason='session_deleted') OR decision IN('pending','started','staged','queued'))`, session)
	return err
}
