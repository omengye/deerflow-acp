package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

const JobBudgetPolicy = "memory-job-budget-v1"
const jobLeaseDuration = 30 * time.Second
const jobBudgetMigration = `
CREATE TABLE memory_job_leases(
 job_id TEXT PRIMARY KEY REFERENCES memory_jobs(id),
 lease_id TEXT NOT NULL DEFAULT '',lease_ns INTEGER NOT NULL DEFAULT 0,
 worker_attempt_id TEXT NOT NULL DEFAULT '',fence INTEGER NOT NULL DEFAULT 0,
 operation_id TEXT NOT NULL DEFAULT '',response TEXT NOT NULL DEFAULT '',
 settlement BLOB,response_saved INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '');
`

func (s *Store) SetJobLedger(ledger *budget.Ledger) { s.ledger = ledger }
func jobMember(id string) string                    { return "memory/" + id }
func (j Job) BudgetScope() budget.Scope {
	return budget.Scope{RootBudgetID: j.RootBudgetID, MemberID: j.MemberID, SessionID: j.SessionID, AttemptID: j.WorkerAttemptID, Fence: j.Fence}
}

type jobLease struct {
	token, attempt, operation, response, errorCode string
	expires, fence                                 int64
	saved                                          bool
	settlement                                     []byte
}

func readJobLease(ctx context.Context, tx *sql.Tx, id string) (jobLease, error) {
	var l jobLease
	err := tx.QueryRowContext(ctx, "SELECT lease_id,lease_ns,worker_attempt_id,fence,operation_id,response,settlement,response_saved,error_code FROM memory_job_leases WHERE job_id=?", id).Scan(&l.token, &l.expires, &l.attempt, &l.fence, &l.operation, &l.response, &l.settlement, &l.saved, &l.errorCode)
	return l, err
}
func storedJob(ctx context.Context, tx *sql.Tx, id string) (Job, string, error) {
	var j Job
	var raw []byte
	var status string
	var runID, inputID, attemptID string
	err := tx.QueryRowContext(ctx, "SELECT payload,result,status,run_id,input_id,attempt_id FROM memory_jobs WHERE id=?", id).Scan(&raw, &j.Result, &status, &runID, &inputID, &attemptID)
	if err != nil {
		return j, status, err
	}
	err = json.Unmarshal(raw, &j)
	if err == nil && (j.ID != id || j.RunID != runID || j.InputID != inputID || j.AttemptID != attemptID) {
		err = harness.ErrInvalidInput
	}
	return j, status, err
}
func (s *Store) leasedJobTx(ctx context.Context, tx *sql.Tx, claim Job, cleanup ...bool) (Job, jobLease, error) {
	j, status, err := storedJob(ctx, tx, claim.ID)
	if err != nil {
		return j, jobLease{}, err
	}
	l, err := readJobLease(ctx, tx, claim.ID)
	if err != nil {
		return j, l, err
	}
	if s.ledger == nil || j.BudgetPolicy != JobBudgetPolicy || j.MemberID != jobMember(j.ID) || (status != "running" && !(len(cleanup) == 1 && cleanup[0])) || claim.ClaimToken == "" || l.token != claim.ClaimToken || l.expires <= time.Now().UnixNano() {
		return j, l, budget.ErrScope
	}
	j.ClaimToken, j.WorkerAttemptID, j.Fence = claim.ClaimToken, l.attempt, l.fence
	return j, l, nil
}

// The runtime composes this with its root-stop gate in the ledger transaction.
func (s *Store) CheckJobBudgetEffectTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
	j, err := s.checkJobBudgetLeaseTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	if err = hydrateJob(ctx, tx, &j); err != nil {
		return err
	}
	return jobSourceReadyTx(ctx, tx, j)
}
func (s *Store) CheckJobBudgetHeartbeatTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
	_, err := s.checkJobBudgetLeaseTx(ctx, tx, scope)
	return err
}
func (s *Store) checkJobBudgetLeaseTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) (Job, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT job_id FROM memory_job_leases WHERE worker_attempt_id=? AND fence=? AND lease_id<>'' AND lease_ns>?", scope.AttemptID, scope.Fence, time.Now().UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, budget.ErrScope
	}
	if err != nil {
		return Job{}, err
	}
	j, status, err := storedJob(ctx, tx, id)
	if err != nil {
		return j, err
	}
	if status != "running" || j.BudgetPolicy != JobBudgetPolicy || j.MemberID != jobMember(id) || j.RootBudgetID != scope.RootBudgetID || j.MemberID != scope.MemberID || j.SessionID != scope.SessionID {
		return j, budget.ErrScope
	}
	var matches bool
	err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_members WHERE id=? AND kind='memory_job' AND root_id=? AND session_id=? AND origin_id=? AND parent_id=?)", j.MemberID, j.RootBudgetID, j.SessionID, j.RunID, j.RunID).Scan(&matches)
	if err != nil {
		return j, err
	}
	if !matches {
		return j, budget.ErrScope
	}
	return j, nil
}
func jobSourceReadyTx(ctx context.Context, tx *sql.Tx, j Job) error {
	if err := budget.CheckTaskTreeRootStoppedTx(ctx, tx, j.RootBudgetID); err != nil {
		return err
	}
	var ready bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_runs r JOIN harness_sessions s ON s.id=r.session_id WHERE r.id=? AND r.input_id=? AND r.session_id=? AND r.status='completed')", j.RunID, j.InputID, j.SessionID).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return harness.ErrExecutionConflict
	}
	explicit, err := explicitActionTx(ctx, tx, j.RunID, j.InputID)
	if err != nil {
		return err
	}
	if explicit {
		return errJobExplicit
	}
	m, err := inputSnapshot(ctx, tx, j.RunID, j.InputID)
	if err != nil {
		return err
	}
	for _, scope := range m.Scopes {
		epoch, e := deletionEpochTx(ctx, tx, scope.Key)
		if e != nil {
			return e
		}
		if epoch != scope.DeleteEpoch {
			return ErrSourceSuperseded
		}
	}
	return nil
}

var errJobExplicit = errors.New("memory job has explicit action")

func finishJobStateTx(ctx context.Context, tx *sql.Tx, j Job, status, reason, decision string, available time.Time) error {
	if _, err := tx.ExecContext(ctx, "UPDATE memory_jobs SET status=?,error_code=?,available_at=?,updated_at=? WHERE id=?", status, reason, available.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), j.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memory_job_leases SET lease_id='',lease_ns=0 WHERE job_id=?", j.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE memory_inputs SET decision=? WHERE input_id=? AND job_id=?", decision, j.InputID, j.ID)
	return err
}
func sourceRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, errJobExplicit):
		return "explicit_action", true
	case errors.Is(err, ErrSourceSuperseded):
		return "superseded", true
	case errors.Is(err, harness.ErrExecutionConflict), errors.Is(err, harness.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return "source_unavailable", true
	case errors.Is(err, harness.ErrTaskTreeStopped):
		return "cancelled", true
	case errors.Is(err, budget.ErrUnknown):
		return "budget_unknown", true
	}
	return "", false
}

func (s *Store) claimBudgetJobTx(ctx context.Context, tx *sql.Tx, j *Job) error {
	if s.ledger == nil {
		return budget.ErrScope
	}
	if err := hydrateJob(ctx, tx, j); err != nil {
		return err
	}
	if err := jobSourceReadyTx(ctx, tx, *j); err != nil {
		reason, ok := sourceRefusal(err)
		if !ok {
			return err
		}
		return finishJobStateTx(ctx, tx, *j, "discarded", reason, reason, time.Now())
	}
	l, err := readJobLease(ctx, tx, j.ID)
	if err != nil {
		return err
	}
	j.ClaimToken = rand.Text()
	if j.Result == "" {
		if err = s.ledger.CheckModelAdmissionTx(ctx, tx, j.RootBudgetID); err != nil {
			var limit *budget.LimitError
			if errors.As(err, &limit) {
				if limit.Temporary {
					return finishJobStateTx(ctx, tx, *j, "queued", "budget_held", "queued", time.Now().Add(time.Second))
				}
				return finishJobStateTx(ctx, tx, *j, "completed", "quota_skip", "quota_skip", time.Now())
			}
			if errors.Is(err, budget.ErrUnknown) {
				return finishJobStateTx(ctx, tx, *j, "blocked", "budget_unknown", "budget_unknown", time.Now())
			}
			return err
		}
		j.Fence = l.fence + 1
		j.WorkerAttemptID = "memory-attempt/" + rand.Text()
		if err = s.ledger.BeginAttemptTx(ctx, tx, j.BudgetScope()); err != nil {
			return err
		}
		j.OperationID = "memory-call/" + j.WorkerAttemptID
		if _, err = tx.ExecContext(ctx, "UPDATE memory_job_leases SET worker_attempt_id=?,fence=?,operation_id=?,response='',settlement=NULL,response_saved=0,error_code='' WHERE job_id=?", j.WorkerAttemptID, j.Fence, j.OperationID, j.ID); err != nil {
			return err
		}
		day := time.Now().UTC().Format("2006-01-02")
		if _, err = tx.ExecContext(ctx, "INSERT INTO memory_daily_attempts(day,attempts) VALUES(?,1) ON CONFLICT(day) DO UPDATE SET attempts=attempts+1", day); err != nil {
			return err
		}
	} else {
		j.WorkerAttemptID, j.Fence, j.OperationID = l.attempt, l.fence, l.operation
	}
	if _, err = tx.ExecContext(ctx, "UPDATE memory_job_leases SET lease_id=?,lease_ns=? WHERE job_id=?", j.ClaimToken, time.Now().Add(jobLeaseDuration).UnixNano(), j.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE memory_jobs SET status='running',attempts=attempts+?,error_code='',updated_at=? WHERE id=?", boolInt(j.Result == ""), time.Now().UTC().Format(time.RFC3339Nano), j.ID)
	return err
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReserveJobCall receives the digest and estimate of the exact message/options
// representation used by the extractor. MarkDispatched is the only I/O permit.
func (s *Store) ReserveJobCall(ctx context.Context, j Job, digest string, input int64, output int) (budget.Reservation, error) {
	if s.ledger == nil || j.ClaimToken == "" {
		return budget.Reservation{}, budget.ErrScope
	}
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, l, e := s.leasedJobTx(ctx, tx, j)
		if e != nil {
			return e
		}
		if stored.Result != "" || l.operation != j.OperationID || stored.BudgetScope() != j.BudgetScope() {
			return budget.ErrScope
		}
		return nil
	})
	if err != nil {
		return budget.Reservation{}, err
	}
	return s.ledger.ReserveModel(ctx, j.BudgetScope(), budget.ModelRequest{OperationID: j.OperationID, Digest: digest, InputTokens: input, MaxOutputTokens: output})
}
func (s *Store) DispatchJobCall(ctx context.Context, j Job, grant budget.Reservation) (bool, error) {
	if s.ledger == nil || grant.Scope != j.BudgetScope() || grant.OperationID != j.OperationID {
		return false, budget.ErrScope
	}
	return s.ledger.MarkDispatched(ctx, grant)
}

type JobResponse struct {
	Raw       string
	Usage     harness.Usage
	Complete  bool
	ErrorCode string
}

// Persist response/usage before settling. No caller can replace already durable
// response evidence, and stale workers cannot write it after their lease ends.
func (s *Store) SaveJobResponse(ctx context.Context, j Job, response JobResponse) error {
	if len(response.Raw) > 16384 {
		return harness.ErrInvalidInput
	}
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, l, err := s.leasedJobTx(ctx, tx, j)
		if err != nil {
			return err
		}
		if stored.BudgetScope() != j.BudgetScope() || l.operation != j.OperationID {
			return budget.ErrScope
		}
		var state string
		if err = tx.QueryRowContext(ctx, "SELECT state FROM budget_reservations WHERE id=? AND attempt_id=?", l.operation, l.attempt).Scan(&state); err != nil {
			return err
		}
		if state != "dispatched" {
			return budget.ErrScope
		}
		settlement := budget.Settlement{Usage: response.Usage, Complete: response.Complete, Unknown: !response.Complete}
		raw, err := json.Marshal(settlement)
		if err != nil {
			return err
		}
		if l.saved {
			if l.response != response.Raw || string(l.settlement) != string(raw) || l.errorCode != response.ErrorCode {
				return budget.ErrConflict
			}
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE memory_job_leases SET response=?,settlement=?,response_saved=1,error_code=? WHERE job_id=?", response.Raw, raw, response.ErrorCode, j.ID)
		return err
	})
}

func (s *Store) settleJobAttemptTx(ctx context.Context, tx *sql.Tx, j Job, l jobLease) (bool, error) {
	if l.attempt == "" {
		return false, nil
	}
	j.WorkerAttemptID, j.Fence = l.attempt, l.fence
	var state string
	err := tx.QueryRowContext(ctx, "SELECT state FROM budget_reservations WHERE id=? AND attempt_id=?", l.operation, l.attempt).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if state == "reserved" {
		if err = s.ledger.ReleaseMemoryReservationTx(ctx, tx, j.BudgetScope(), l.operation); err != nil {
			return false, err
		}
	}
	var settlement *budget.Settlement
	if l.saved {
		settlement = &budget.Settlement{}
		if err = json.Unmarshal(l.settlement, settlement); err != nil {
			return false, err
		}
	}
	return s.ledger.FinishMemoryAttemptTx(ctx, tx, j.BudgetScope(), l.operation, settlement)
}

func (s *Store) SettleJobResponse(ctx context.Context, j Job) error {
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, l, err := s.leasedJobTx(ctx, tx, j)
		if err != nil {
			return err
		}
		if !l.saved {
			return budget.ErrUnknown
		}
		unknown, err := s.settleJobAttemptTx(ctx, tx, stored, l)
		if err != nil {
			return err
		}
		if unknown {
			return finishJobStateTx(ctx, tx, stored, "blocked", "budget_unknown", "budget_unknown", time.Now())
		}
		return s.publishJobResponseTx(ctx, tx, stored, l, false)
	})
}
func (s *Store) publishJobResponseTx(ctx context.Context, tx *sql.Tx, j Job, l jobLease, recovering bool) error {
	reason := l.errorCode
	if reason == "" {
		if _, err := extractionEnvelope([]byte(l.response)); err != nil {
			reason = "invalid_envelope"
		}
	}
	if reason != "" {
		var attempts int
		if err := tx.QueryRowContext(ctx, "SELECT attempts FROM memory_jobs WHERE id=?", j.ID).Scan(&attempts); err != nil {
			return err
		}
		status := "queued"
		if attempts >= 3 {
			status = "failed"
		}
		return finishJobStateTx(ctx, tx, j, status, reason, status, time.Now().Add(time.Minute))
	}
	if err := saveExtractionResultTx(ctx, tx, j.ID, j.RunID, j.InputID, l.response, j.Provenance); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memory_jobs SET result=? WHERE id=?", l.response, j.ID); err != nil {
		return err
	}
	if recovering {
		return finishJobStateTx(ctx, tx, j, "queued", "result_replay", "queued", time.Now())
	}
	return nil
}

func (s *Store) HeartbeatJob(ctx context.Context, j Job) error {
	var reason string
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, l, err := s.leasedJobTx(ctx, tx, j)
		if err != nil {
			return err
		}
		if l.attempt != "" {
			var state string
			if err = tx.QueryRowContext(ctx, "SELECT state FROM budget_attempts WHERE id=?", l.attempt).Scan(&state); err != nil {
				return err
			}
			if state == "active" {
				if err = s.ledger.HeartbeatTx(ctx, tx, stored.BudgetScope()); err != nil {
					return err
				}
			}
		}
		if err = tx.QueryRowContext(ctx, "SELECT blocked_reason FROM budget_roots WHERE id=?", j.RootBudgetID).Scan(&reason); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE memory_job_leases SET lease_ns=? WHERE job_id=?", time.Now().Add(jobLeaseDuration).UnixNano(), j.ID)
		return err
	})
	if err != nil {
		return err
	}
	if reason == "unknown" {
		return budget.ErrUnknown
	}
	if reason != "" {
		return &budget.LimitError{Resource: reason}
	}
	return nil
}

// Call only while the host owns its exclusive database process lock, before
// generic budget reconciliation. Expired live leases use the same recovery.
func (s *Store) RecoverJobs(ctx context.Context) error { return s.recoverBudgetJobs(ctx, true) }
func (s *Store) recoverBudgetJobs(ctx context.Context, startup bool) error {
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		if startup {
			if err := s.reconcileSkippedInputsTx(ctx, tx); err != nil {
				return err
			}
		}
		if err := discardLegacyJobsTx(ctx, tx); err != nil {
			return err
		}
		if err := discardFailedSourcesTx(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT j.id FROM memory_jobs j JOIN memory_job_leases l ON l.job_id=j.id WHERE l.lease_id<>'' AND (? OR l.lease_ns<=?)", startup, time.Now().UnixNano())
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			j, previous, err := storedJob(ctx, tx, id)
			if err == nil {
				err = hydrateJob(ctx, tx, &j)
			}
			if err != nil {
				if !sourceIntegrityError(err) {
					return err
				}
				if err = s.discardCorruptJobTx(ctx, tx, id); err != nil {
					return err
				}
				continue
			}
			l, err := readJobLease(ctx, tx, id)
			if err != nil {
				return err
			}
			if s.ledger == nil {
				return budget.ErrScope
			}
			unknown, err := s.settleJobAttemptTx(ctx, tx, j, l)
			if err != nil {
				return err
			}
			switch {
			case unknown:
				err = finishJobStateTx(ctx, tx, j, "blocked", "budget_unknown", "budget_unknown", time.Now())
			case previous != "running":
				err = finishJobStateTx(ctx, tx, j, previous, "cancelled", previous, time.Now())
			case l.saved:
				err = s.publishJobResponseTx(ctx, tx, j, l, true)
			default:
				var attempts int
				if err = tx.QueryRowContext(ctx, "SELECT attempts FROM memory_jobs WHERE id=?", id).Scan(&attempts); err != nil {
					return err
				}
				status := "queued"
				if attempts >= 3 {
					status = "failed"
				}
				err = finishJobStateTx(ctx, tx, j, status, "worker_restarted", status, time.Now())
			}
			if err != nil {
				return err
			}
		}
		return discardFailedSourcesTx(ctx, tx)
	})
}

// Abort owns cleanup only. It cannot release a dispatched/unknown call as free.
func (s *Store) AbortJob(ctx context.Context, j Job, cause error) error {
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, l, err := s.leasedJobTx(ctx, tx, j, true)
		if err != nil {
			return err
		}
		unknown, err := s.settleJobAttemptTx(ctx, tx, stored, l)
		if err != nil {
			return err
		}
		if unknown {
			return finishJobStateTx(ctx, tx, stored, "blocked", "budget_unknown", "budget_unknown", time.Now())
		}
		_, previous, err := storedJob(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if previous != "running" {
			return finishJobStateTx(ctx, tx, stored, previous, "cancelled", previous, time.Now())
		}
		if l.saved {
			return s.publishJobResponseTx(ctx, tx, stored, l, true)
		}
		var limit *budget.LimitError
		if errors.As(cause, &limit) {
			if limit.Temporary {
				// No reservation/dispatch was admitted in this race. Keep the job
				// eligible after competing work joins; the UTC admission still counts.
				var admitted bool
				if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_reservations WHERE id=?)", l.operation).Scan(&admitted); err != nil {
					return err
				}
				if !admitted {
					if _, err = tx.ExecContext(ctx, "UPDATE memory_jobs SET attempts=max(0,attempts-1) WHERE id=?", stored.ID); err != nil {
						return err
					}
				}
				return finishJobStateTx(ctx, tx, stored, "queued", "budget_held", "queued", time.Now().Add(time.Second))
			}
			return finishJobStateTx(ctx, tx, stored, "completed", "quota_skip", "quota_skip", time.Now())
		}
		if reason, ok := sourceRefusal(cause); ok {
			status := "discarded"
			if reason == "budget_unknown" {
				status = "blocked"
			}
			return finishJobStateTx(ctx, tx, stored, status, reason, reason, time.Now())
		}
		var attempts int
		if err = tx.QueryRowContext(ctx, "SELECT attempts FROM memory_jobs WHERE id=?", stored.ID).Scan(&attempts); err != nil {
			return err
		}
		status := "queued"
		if attempts >= 3 {
			status = "failed"
		}
		return finishJobStateTx(ctx, tx, stored, status, "call_not_dispatched", status, time.Now().Add(time.Minute))
	})
}
