package skillmaintenance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxAttempts = 3
const leaseDuration = 15 * time.Second

type Store struct {
	db       *sql.DB
	policy   Policy
	now      func() time.Time
	admit    AdmissionCheck
	terminal TerminalHook
}

// AdmissionCheck runs in the reservation transaction before any provider I/O.
// It must use only this transaction and immutable host metadata.
type AdmissionCheck func(context.Context, *sql.Tx, Job) error

// TerminalHook commits producer metadata cleanup with a terminal job state.
// It must not perform I/O outside this SQL transaction.
type TerminalHook func(context.Context, *sql.Tx, Job) error
type Hooks struct {
	Admission AdmissionCheck
	Terminal  TerminalHook
}

func New(ctx context.Context, db *sql.DB, policy Policy) (*Store, error) {
	return NewWithAdmission(ctx, db, policy, nil)
}

func NewWithAdmission(ctx context.Context, db *sql.DB, policy Policy, admit AdmissionCheck) (*Store, error) {
	return NewWithHooks(ctx, db, policy, Hooks{Admission: admit})
}

func NewWithHooks(ctx context.Context, db *sql.DB, policy Policy, hooks Hooks) (*Store, error) {
	if db == nil || !validPolicy(policy) {
		return nil, ErrInvalid
	}
	s := &Store{db: db, policy: policy, now: time.Now, admit: hooks.Admission, terminal: hooks.Terminal}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = Migrate(ctx, tx); err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

func Migrate(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_maintenance_clock (id INTEGER PRIMARY KEY CHECK(id=1),last_ns INTEGER NOT NULL);
INSERT OR IGNORE INTO df_skill_maintenance_clock VALUES(1,0);
CREATE TABLE IF NOT EXISTS df_skill_maintenance_days (
 day TEXT PRIMARY KEY,model_calls INTEGER NOT NULL CHECK(model_calls>=0),charged_tokens INTEGER NOT NULL CHECK(charged_tokens>=0));
CREATE TABLE IF NOT EXISTS df_skill_maintenance_jobs (
 id TEXT PRIMARY KEY,source_key TEXT NOT NULL,name TEXT NOT NULL,request_key TEXT NOT NULL,reference TEXT NOT NULL,input_digest TEXT NOT NULL,
 policy BLOB NOT NULL,state TEXT NOT NULL,state_version INTEGER NOT NULL,attempts INTEGER NOT NULL,
 token TEXT NOT NULL DEFAULT '',lease_ns INTEGER NOT NULL DEFAULT 0,deadline_ns INTEGER NOT NULL DEFAULT 0,retry_ns INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',
 UNIQUE(source_key,name,request_key));
CREATE INDEX IF NOT EXISTS df_skill_maintenance_queue ON df_skill_maintenance_jobs(state,retry_ns,id);
CREATE TABLE IF NOT EXISTS df_skill_maintenance_calls (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,job_id TEXT NOT NULL REFERENCES df_skill_maintenance_jobs(id),attempt INTEGER NOT NULL,claim_token TEXT NOT NULL,
 day TEXT NOT NULL REFERENCES df_skill_maintenance_days(day),stage TEXT NOT NULL,model_identity TEXT NOT NULL,input_digest TEXT NOT NULL,
 reserved_tokens INTEGER NOT NULL,charged_tokens INTEGER NOT NULL,input_tokens INTEGER NOT NULL DEFAULT 0,output_tokens INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL,estimated INTEGER NOT NULL DEFAULT 1,settlement_digest TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS df_skill_maintenance_job_calls ON df_skill_maintenance_calls(job_id,id);
`)
	return err
}

func bounded(v string, n int) bool {
	return v != "" && len(v) <= n && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func digest(v string) bool {
	if len(v) != 64 || strings.ToLower(v) != v {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func timestamp(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// A backward clock cannot move billing to an earlier day or revive a lease.
func (s *Store) clock(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_clock SET last_ns=? WHERE id=1 AND last_ns<=?`, now.UnixNano(), now.UnixNano())
	if err != nil {
		return now, err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		err = ErrClock
	}
	return now, err
}

type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readJob(ctx context.Context, db rowQuery, id string) (Job, string, int64, error) {
	var j Job
	var token string
	var lease, deadline, retry int64
	var policy []byte
	err := db.QueryRowContext(ctx, `SELECT id,source_key,name,request_key,reference,input_digest,policy,state,state_version,attempts,token,lease_ns,deadline_ns,retry_ns,error_code FROM df_skill_maintenance_jobs WHERE id=?`, id).
		Scan(&j.ID, &j.Request.SourceIdentity, &j.Request.Name, &j.Request.Key, &j.Request.Reference, &j.Request.InputDigest, &policy, &j.State, &j.StateVersion, &j.Attempts, &token, &lease, &deadline, &retry, &j.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return j, token, lease, harness.ErrNotFound
	}
	if err != nil {
		return j, token, lease, err
	}
	if json.Unmarshal(policy, &j.Policy) != nil || !validPolicy(j.Policy) || j.StateVersion < 1 || j.Attempts < 0 || j.Attempts > maxAttempts || !validRequest(j.Request) {
		return Job{}, "", 0, ErrInvalid
	}
	j.Deadline, j.RetryAt = timestamp(deadline), timestamp(retry)
	return j, token, lease, nil
}

func validRequest(r Request) bool {
	return digest(r.SourceIdentity) && harness.ValidSkillName(r.Name) && bounded(r.Key, 256) && bounded(r.Reference, 256) && digest(r.InputDigest)
}

func (s *Store) Enqueue(ctx context.Context, r Request) (Job, error) {
	if !validRequest(r) {
		return Job{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j, err := s.EnqueueTx(ctx, tx, r)
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

// EnqueueTx lets the producer save its input snapshot in the same transaction.
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, r Request) (Job, error) {
	if !validRequest(r) {
		return Job{}, ErrInvalid
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM df_skill_maintenance_jobs WHERE source_key=? AND name=? AND request_key=?`, r.SourceIdentity, r.Name, r.Key).Scan(&id)
	if err == nil {
		j, _, _, err := readJob(ctx, tx, id)
		if err == nil && j.Request != r {
			err = ErrConflict
		}
		return j, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_maintenance_jobs WHERE state IN ('queued','running','retry')`).Scan(&pending); err != nil {
		return Job{}, err
	}
	if pending >= 100 {
		return Job{}, ErrBudget
	}
	j := Job{ID: "smj_" + rand.Text(), Request: r, Policy: s.policy, State: "queued", StateVersion: 1}
	policy, _ := json.Marshal(j.Policy)
	_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_maintenance_jobs(id,source_key,name,request_key,reference,input_digest,policy,state,state_version,attempts) VALUES(?,?,?,?,?,?,?,?,1,0)`, j.ID, r.SourceIdentity, r.Name, r.Key, r.Reference, r.InputDigest, policy, j.State)
	if err != nil {
		return Job{}, err
	}
	return j, nil
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return ReadJob(ctx, s.db, id)
}

func ReadJob(ctx context.Context, db rowQuery, id string) (Job, error) {
	j, _, _, err := readJob(ctx, db, id)
	return j, err
}

// GuardTx fences producer stage commits, including proposal insertion and
// validation results. It does not repeat admission checks: a result may record
// that the pinned head became stale during the already charged call.
func GuardTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	b, ok := ctx.Value(brokerKey{}).(*claimBroker)
	if !ok || b.claim.Job.ID != jobID {
		return ErrConflict
	}
	now, err := b.store.clock(ctx, tx)
	if err != nil {
		return err
	}
	_, err = b.store.check(ctx, tx, b.claim, now)
	return err
}

// Claim persists both concurrency admission and the first-start deadline.
// Expired workers keep all charged calls and attempts. Recovery is bounded.
func (s *Store) Claim(ctx context.Context) (Claim, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return Claim{}, false, err
	}
	var expired []string
	if s.terminal != nil {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_maintenance_jobs WHERE (state='running' AND lease_ns<=?) OR (state IN ('queued','retry') AND deadline_ns>0 AND deadline_ns<=?)`, now.UnixNano(), now.UnixNano())
		if err != nil {
			return Claim{}, false, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			expired = append(expired, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return Claim{}, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state=CASE WHEN attempts>=? OR deadline_ns<=? THEN 'failed' ELSE 'retry' END,
 state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='worker_interrupted' WHERE state='running' AND lease_ns<=?`, maxAttempts, now.UnixNano(), now.UnixNano())
	if err != nil {
		return Claim{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='failed',state_version=state_version+1,error_code='job_deadline',retry_ns=0 WHERE state IN ('queued','retry') AND deadline_ns>0 AND deadline_ns<=?`, now.UnixNano())
	if err != nil {
		return Claim{}, false, err
	}
	for _, id := range expired {
		job, err := ReadJob(ctx, tx, id)
		if err != nil {
			return Claim{}, false, err
		}
		if job.State == "failed" {
			if err = s.terminal(ctx, tx, job); err != nil {
				return Claim{}, false, err
			}
		}
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_maintenance_jobs WHERE state='running'`).Scan(&active); err != nil {
		return Claim{}, false, err
	}
	if active >= s.policy.Workers {
		return Claim{}, false, tx.Commit()
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM df_skill_maintenance_jobs WHERE state IN ('queued','retry') AND retry_ns<=? AND attempts<? ORDER BY rowid LIMIT 1`, now.UnixNano(), maxAttempts).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, tx.Commit()
	}
	if err != nil {
		return Claim{}, false, err
	}
	j, _, _, err := readJob(ctx, tx, id)
	if err != nil {
		return Claim{}, false, err
	}
	if active >= j.Policy.Workers {
		return Claim{}, false, tx.Commit()
	}
	if j.Deadline.IsZero() {
		j.Deadline = now.Add(time.Duration(min(j.Policy.MaxJobSeconds, s.policy.MaxJobSeconds)) * time.Second)
	}
	j.State, j.StateVersion, j.Attempts, j.ErrorCode, j.RetryAt = "running", j.StateVersion+1, j.Attempts+1, "", time.Time{}
	claim := Claim{Job: j, token: rand.Text()}
	lease := min(now.Add(leaseDuration).UnixNano(), j.Deadline.UnixNano())
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='running',state_version=?,attempts=?,token=?,lease_ns=?,deadline_ns=?,retry_ns=0,error_code='' WHERE id=?`, j.StateVersion, j.Attempts, claim.token, lease, j.Deadline.UnixNano(), j.ID)
	if err != nil {
		return Claim{}, false, err
	}
	return claim, true, tx.Commit()
}

func (s *Store) check(ctx context.Context, tx *sql.Tx, c Claim, now time.Time) (Job, error) {
	j, token, lease, err := readJob(ctx, tx, c.Job.ID)
	if err != nil {
		return j, err
	}
	if c.token == "" || token != c.token || j.State != "running" || j.StateVersion != c.Job.StateVersion || j.Attempts != c.Job.Attempts || !j.Deadline.After(now) || lease <= now.UnixNano() {
		return j, ErrConflict
	}
	return j, nil
}

func (s *Store) Heartbeat(ctx context.Context, c Claim) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return err
	}
	j, err := s.check(ctx, tx, c, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET lease_ns=? WHERE id=?`, min(now.Add(leaseDuration).UnixNano(), j.Deadline.UnixNano()), j.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Complete/Fail never release model charges. Error codes are a fixed enum.
func (s *Store) Finish(ctx context.Context, c Claim, code string, retry bool) error {
	switch code {
	case "", "model_failed", "invalid_candidate", "budget_exhausted", "storage_error", "cancelled", "job_deadline", "source_unavailable", "base_changed", "configuration_changed", "evidence_changed":
	default:
		return ErrInvalid
	}
	if code == "" && retry {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return err
	}
	j, token, lease, err := readJob(ctx, tx, c.Job.ID)
	if err != nil {
		return err
	}
	if c.token == "" || token != c.token || j.State != "running" || j.StateVersion != c.Job.StateVersion || j.Attempts != c.Job.Attempts {
		return ErrConflict
	}
	if !j.Deadline.After(now) || lease <= now.UnixNano() {
		if code == "" {
			return ErrConflict
		}
		if !j.Deadline.After(now) {
			code, retry = "job_deadline", false
		}
	}
	var unsettled int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=? AND claim_token=? AND state='reserved'`, j.ID, c.token).Scan(&unsettled); err != nil {
		return err
	}
	if code == "" && unsettled != 0 {
		return ErrConflict
	}
	state, at := "completed", int64(0)
	if code != "" {
		state = "failed"
	}
	if retry && j.Attempts < maxAttempts {
		backoff := now.Add(time.Duration(1<<uint(j.Attempts)) * time.Second)
		if backoff.Before(j.Deadline) {
			state, at = "retry", backoff.UnixNano()
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state=?,state_version=state_version+1,token='',lease_ns=0,retry_ns=?,error_code=? WHERE id=?`, state, at, code, j.ID)
	if err != nil {
		return err
	}
	if s.terminal != nil && (state == "failed" || state == "completed") {
		job, err := ReadJob(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if err = s.terminal(ctx, tx, job); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Cancel(ctx context.Context, id string, version int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = CancelTx(ctx, tx, id, version); err != nil {
		return err
	}
	return tx.Commit()
}

func CancelTx(ctx context.Context, tx *sql.Tx, id string, version int64) error {
	if version < 1 {
		return ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled' WHERE id=? AND state_version=? AND state IN ('queued','retry','running')`, id, version)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}
