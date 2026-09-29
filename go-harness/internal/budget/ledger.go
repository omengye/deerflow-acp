package budget

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type Ledger struct {
	db     *sql.DB
	config Config
}

func New(db *sql.DB, config Config) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("budget database is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 30 * time.Second
	}
	if config.LeaseDuration < time.Millisecond {
		return nil, errors.New("budget lease duration must be positive")
	}
	l := &Ledger{db: db, config: config}
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS budget_roots (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, root_run_id TEXT NOT NULL,
 policy_hash TEXT NOT NULL, limits_json BLOB NOT NULL,
 model_calls INTEGER NOT NULL DEFAULT 0, tool_calls INTEGER NOT NULL DEFAULT 0,
 spent_tokens INTEGER NOT NULL DEFAULT 0, held_tokens INTEGER NOT NULL DEFAULT 0,
 elapsed_ns INTEGER NOT NULL DEFAULT 0, last_ns INTEGER NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1, blocked_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS budget_members (
 id TEXT PRIMARY KEY, root_id TEXT NOT NULL REFERENCES budget_roots(id),
 session_id TEXT NOT NULL, origin_id TEXT NOT NULL, parent_id TEXT NOT NULL, kind TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS budget_attempts (
 id TEXT PRIMARY KEY, root_id TEXT NOT NULL REFERENCES budget_roots(id),
 member_id TEXT NOT NULL REFERENCES budget_members(id), session_id TEXT NOT NULL,
 fence INTEGER NOT NULL, lease_ns INTEGER NOT NULL, state TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '',
 UNIQUE(member_id, fence)
);
CREATE INDEX IF NOT EXISTS budget_attempt_root ON budget_attempts(root_id, state);
CREATE TABLE IF NOT EXISTS budget_reservations (
 id TEXT PRIMARY KEY, root_id TEXT NOT NULL REFERENCES budget_roots(id),
 member_id TEXT NOT NULL, session_id TEXT NOT NULL, attempt_id TEXT NOT NULL REFERENCES budget_attempts(id),
 fence INTEGER NOT NULL, kind TEXT NOT NULL, digest TEXT NOT NULL,
 input_tokens INTEGER NOT NULL, output_cap INTEGER NOT NULL, held_tokens INTEGER NOT NULL,
 state TEXT NOT NULL, settlement_digest TEXT NOT NULL DEFAULT '', usage_json BLOB
);
CREATE INDEX IF NOT EXISTS budget_reservation_root ON budget_reservations(root_id, state);
`)
	if err != nil {
		return nil, &PersistenceError{Operation: "migrate", Err: err}
	}
	if err = migrateToolContinuations(db); err != nil {
		return nil, persist("migrate tool continuations", err)
	}
	return l, nil
}

func persist(op string, err error) error {
	if err == nil {
		return nil
	}
	var p *PersistenceError
	var limit *LimitError
	if errors.As(err, &p) || errors.As(err, &limit) || errors.Is(err, ErrScope) || errors.Is(err, ErrConflict) || errors.Is(err, ErrUnknown) || errors.Is(err, ErrClock) {
		return err
	}
	return &PersistenceError{Operation: op, Err: err}
}

func (l *Ledger) transaction(ctx context.Context, op string, fn func(*sql.Tx) error) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return persist(op, err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return persist(op, err)
	}
	return persist(op, tx.Commit())
}

type root struct {
	Snapshot
	last int64
}

func readRoot(ctx context.Context, tx *sql.Tx, id string) (*root, error) {
	r := &root{}
	var raw []byte
	var elapsed int64
	err := tx.QueryRowContext(ctx, `SELECT id,session_id,root_run_id,policy_hash,limits_json,model_calls,tool_calls,spent_tokens,held_tokens,elapsed_ns,last_ns,revision,blocked_reason FROM budget_roots WHERE id=?`, id).Scan(&r.RootBudgetID, &r.SessionID, &r.RootRunID, &r.PolicyHash, &raw, &r.ModelCalls, &r.ToolCalls, &r.SpentTokens, &r.HeldTokens, &elapsed, &r.last, &r.Revision, &r.BlockedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrScope
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &r.Limits)
	r.Elapsed = time.Duration(elapsed)
	return r, err
}
func saveRoot(ctx context.Context, tx *sql.Tx, r *root) error {
	r.Revision++
	_, err := tx.ExecContext(ctx, `UPDATE budget_roots SET model_calls=?,tool_calls=?,spent_tokens=?,held_tokens=?,elapsed_ns=?,last_ns=?,revision=?,blocked_reason=? WHERE id=?`, r.ModelCalls, r.ToolCalls, r.SpentTokens, r.HeldTokens, int64(r.Elapsed), r.last, r.Revision, r.BlockedReason, r.RootBudgetID)
	return err
}
func validScope(s Scope) bool {
	return s.RootBudgetID != "" && s.MemberID != "" && s.SessionID != "" && s.AttemptID != "" && s.Fence > 0
}

func (l *Ledger) CreateRootTx(ctx context.Context, tx *sql.Tx, spec RootSpec) (err error) {
	defer func() { err = persist("create root", err) }()
	if spec.RootBudgetID == "" || spec.RootRunID == "" || spec.SessionID == "" {
		return ErrScope
	}
	b := spec.Limits
	if b.MaxModelCalls < 0 || b.MaxToolCalls < 0 || b.MaxTokens < 0 || b.MaxOutputTokens < 0 || b.Timeout < 0 {
		return ErrScope
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if spec.PolicyHash == "" {
		spec.PolicyHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	old, err := readRoot(ctx, tx, spec.RootBudgetID)
	if err == nil {
		if old.RootRunID != spec.RootRunID || old.SessionID != spec.SessionID || old.PolicyHash != spec.PolicyHash || old.Limits != b {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, ErrScope) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_roots(id,session_id,root_run_id,policy_hash,limits_json,last_ns) VALUES(?,?,?,?,?,?)`, spec.RootBudgetID, spec.SessionID, spec.RootRunID, spec.PolicyHash, raw, l.config.Now().UnixNano())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_members(id,root_id,session_id,origin_id,parent_id,kind) VALUES(?,?,?,?,?,'run')`, spec.RootRunID, spec.RootBudgetID, spec.SessionID, spec.RootRunID, "")
	return err
}
func (l *Ledger) CreateRoot(ctx context.Context, s RootSpec) error {
	return l.transaction(ctx, "create root", func(tx *sql.Tx) error { return l.CreateRootTx(ctx, tx, s) })
}

func (l *Ledger) BindMemberTx(ctx context.Context, tx *sql.Tx, b MemberBinding) (err error) {
	defer func() { err = persist("bind member", err) }()
	if b.RootBudgetID == "" || b.MemberID == "" || b.SessionID == "" || b.OriginRunID == "" {
		return ErrScope
	}
	var originRoot, originSession string
	if err = tx.QueryRowContext(ctx, `SELECT root_id,session_id FROM budget_members WHERE id=?`, b.OriginRunID).Scan(&originRoot, &originSession); errors.Is(err, sql.ErrNoRows) {
		return ErrScope
	} else if err != nil {
		return err
	}
	if originRoot != b.RootBudgetID {
		return ErrConflict
	}
	if b.Kind == "" {
		b.Kind = "run"
	}
	if b.Kind == "run" && originSession != b.SessionID {
		return ErrScope
	}
	if b.Kind != "run" && b.ParentMemberID != b.OriginRunID {
		return ErrScope
	}
	if b.ParentMemberID != "" {
		var parentRoot string
		if err = tx.QueryRowContext(ctx, `SELECT root_id FROM budget_members WHERE id=?`, b.ParentMemberID).Scan(&parentRoot); errors.Is(err, sql.ErrNoRows) {
			return ErrScope
		} else if err != nil {
			return err
		}
		if parentRoot != b.RootBudgetID {
			return ErrConflict
		}
	}
	var old MemberBinding
	err = tx.QueryRowContext(ctx, `SELECT root_id,id,session_id,origin_id,parent_id,kind FROM budget_members WHERE id=?`, b.MemberID).Scan(&old.RootBudgetID, &old.MemberID, &old.SessionID, &old.OriginRunID, &old.ParentMemberID, &old.Kind)
	if err == nil {
		if old != b {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_members(id,root_id,session_id,origin_id,parent_id,kind) VALUES(?,?,?,?,?,?)`, b.MemberID, b.RootBudgetID, b.SessionID, b.OriginRunID, b.ParentMemberID, b.Kind)
	return err
}
func (l *Ledger) BindMember(ctx context.Context, b MemberBinding) error {
	return l.transaction(ctx, "bind member", func(tx *sql.Tx) error { return l.BindMemberTx(ctx, tx, b) })
}

// advance accounts the union of active attempts, bounded by issued leases.
func (l *Ledger) advance(ctx context.Context, tx *sql.Tx, r *root, now int64) error {
	if now < r.last {
		return ErrClock
	}
	var first, last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(lease_ns),MAX(lease_ns) FROM budget_attempts WHERE root_id=? AND state='active'`, r.RootBudgetID).Scan(&first, &last); err != nil {
		return err
	}
	if last.Valid {
		end := min(now, last.Int64)
		if end > r.last {
			r.Elapsed += time.Duration(end - r.last)
		}
	}
	r.last = now
	if first.Valid && first.Int64 <= now {
		if _, err := tx.ExecContext(ctx, `UPDATE budget_reservations SET state='unknown' WHERE state IN ('reserved','dispatched') AND attempt_id IN (SELECT id FROM budget_attempts WHERE root_id=? AND state='active' AND lease_ns<=?)`, r.RootBudgetID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE budget_attempts SET state='unknown',outcome='interrupted' WHERE root_id=? AND state='active' AND lease_ns<=?`, r.RootBudgetID, now); err != nil {
			return err
		}
		r.BlockedReason = "unknown"
	}
	if r.Limits.Timeout > 0 && r.Elapsed >= r.Limits.Timeout && r.BlockedReason == "" {
		r.BlockedReason = "time"
	}
	return nil
}
func blocked(r *root) error {
	switch r.BlockedReason {
	case "":
		return nil
	case "unknown":
		return ErrUnknown
	default:
		return &LimitError{Resource: r.BlockedReason}
	}
}

func (l *Ledger) BeginAttemptTx(ctx context.Context, tx *sql.Tx, s Scope) (err error) {
	defer func() { err = persist("begin attempt", err) }()
	if !validScope(s) {
		return ErrScope
	}
	r, err := readRoot(ctx, tx, s.RootBudgetID)
	if err != nil {
		return err
	}
	now := l.config.Now().UnixNano()
	if err = l.advance(ctx, tx, r, now); err != nil {
		return err
	}
	if err = blocked(r); err != nil {
		return err
	}
	var rid, sid string
	if err = tx.QueryRowContext(ctx, `SELECT root_id,session_id FROM budget_members WHERE id=?`, s.MemberID).Scan(&rid, &sid); errors.Is(err, sql.ErrNoRows) {
		return ErrScope
	} else if err != nil {
		return err
	}
	if rid != s.RootBudgetID || sid != s.SessionID {
		return ErrScope
	}
	var old Scope
	var state string
	err = tx.QueryRowContext(ctx, `SELECT root_id,member_id,session_id,id,fence,state FROM budget_attempts WHERE id=?`, s.AttemptID).Scan(&old.RootBudgetID, &old.MemberID, &old.SessionID, &old.AttemptID, &old.Fence, &state)
	if err == nil {
		if old != s || state != "active" {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var fence int64
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(fence),0),COALESCE(SUM(CASE WHEN state IN ('active','unknown') THEN 1 ELSE 0 END),0) FROM budget_attempts WHERE member_id=?`, s.MemberID).Scan(&fence, &active); err != nil {
		return err
	}
	if s.Fence <= fence || active > 0 {
		return ErrScope
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_attempts(id,root_id,member_id,session_id,fence,lease_ns,state) VALUES(?,?,?,?,?,?,'active')`, s.AttemptID, s.RootBudgetID, s.MemberID, s.SessionID, s.Fence, now+int64(l.config.LeaseDuration))
	if err != nil {
		return err
	}
	return saveRoot(ctx, tx, r)
}
func (l *Ledger) BeginAttempt(ctx context.Context, s Scope) error {
	return l.transaction(ctx, "begin attempt", func(tx *sql.Tx) error { return l.BeginAttemptTx(ctx, tx, s) })
}

func checkOwner(ctx context.Context, tx *sql.Tx, s Scope, live bool) error {
	if !validScope(s) {
		return ErrScope
	}
	var old Scope
	var state string
	err := tx.QueryRowContext(ctx, `SELECT root_id,member_id,session_id,id,fence,state FROM budget_attempts WHERE id=?`, s.AttemptID).Scan(&old.RootBudgetID, &old.MemberID, &old.SessionID, &old.AttemptID, &old.Fence, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrScope
	}
	if err != nil {
		return err
	}
	if old != s || (live && state != "active") {
		return ErrScope
	}
	return nil
}
func (l *Ledger) checkEffect(ctx context.Context, tx *sql.Tx, s Scope) error {
	if err := checkOwner(ctx, tx, s, true); err != nil {
		return err
	}
	if l.config.CheckEffectTx != nil {
		return l.config.CheckEffectTx(ctx, tx, s)
	}
	return nil
}
func (l *Ledger) Validate(ctx context.Context, s Scope) error {
	return l.transaction(ctx, "validate", func(tx *sql.Tx) error {
		r, err := readRoot(ctx, tx, s.RootBudgetID)
		if err != nil {
			return err
		}
		if err = l.advance(ctx, tx, r, l.config.Now().UnixNano()); err != nil {
			return err
		}
		if err = blocked(r); err != nil {
			return err
		}
		if err = l.checkEffect(ctx, tx, s); err != nil {
			return err
		}
		return saveRoot(ctx, tx, r)
	})
}
func (l *Ledger) Heartbeat(ctx context.Context, s Scope) error {
	var denial error
	err := l.transaction(ctx, "heartbeat", func(tx *sql.Tx) error {
		if err := l.HeartbeatTx(ctx, tx, s); err != nil {
			return err
		}
		r, err := readRoot(ctx, tx, s.RootBudgetID)
		if err != nil {
			return err
		}
		// Even when quota has closed admissions, cleanup owns a live lease until
		// real I/O joins. Commit the renewed lease before reporting the limit.
		denial = blocked(r)
		return nil
	})
	if err != nil {
		return err
	}
	return denial
}

// HeartbeatTx renews an owned attempt even after quota exhaustion so the host
// can join real cleanup. Limits are reported by Heartbeat, Snapshot and effect
// admission, not as an error that would roll back this host lease transaction.
func (l *Ledger) HeartbeatTx(ctx context.Context, tx *sql.Tx, s Scope) (err error) {
	defer func() { err = persist("heartbeat", err) }()
	r, err := readRoot(ctx, tx, s.RootBudgetID)
	if err != nil {
		return err
	}
	now := l.config.Now().UnixNano()
	if err = l.advance(ctx, tx, r, now); err != nil {
		return err
	}
	if err = checkOwner(ctx, tx, s, true); err != nil {
		return err
	}
	if l.config.CheckHeartbeatTx != nil {
		if err = l.config.CheckHeartbeatTx(ctx, tx, s); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE budget_attempts SET lease_ns=? WHERE id=?`, now+int64(l.config.LeaseDuration), s.AttemptID); err != nil {
		return err
	}
	return saveRoot(ctx, tx, r)
}
func (l *Ledger) HeartbeatInterval() time.Duration { return l.config.LeaseDuration / 3 }

func (l *Ledger) EndAttemptTx(ctx context.Context, tx *sql.Tx, s Scope, outcome Outcome) (err error) {
	defer func() { err = persist("end attempt", err) }()
	switch outcome {
	case OutcomeCompleted, OutcomeFailed, OutcomeCancelled, OutcomeWaitingInput, OutcomeInterrupted:
	default:
		return ErrScope
	}
	if err = checkOwner(ctx, tx, s, false); err != nil {
		return err
	}
	r, err := readRoot(ctx, tx, s.RootBudgetID)
	if err != nil {
		return err
	}
	if err = l.advance(ctx, tx, r, l.config.Now().UnixNano()); err != nil {
		return err
	}
	var attemptState, priorOutcome string
	if err = tx.QueryRowContext(ctx, `SELECT state,outcome FROM budget_attempts WHERE id=?`, s.AttemptID).Scan(&attemptState, &priorOutcome); err != nil {
		return err
	}
	if attemptState == "ended" {
		if priorOutcome != string(outcome) {
			return ErrConflict
		}
		return nil
	}
	if attemptState == "unknown" && outcome != OutcomeInterrupted {
		return ErrUnknown
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM budget_reservations WHERE attempt_id=? AND state IN ('reserved','dispatched','unknown')`, s.AttemptID).Scan(&count); err != nil {
		return err
	}
	state := "ended"
	if count > 0 && outcome != OutcomeInterrupted {
		return ErrUnknown
	}
	if count > 0 || outcome == OutcomeInterrupted {
		state = "unknown"
		r.BlockedReason = "unknown"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE budget_attempts SET state=?,outcome=? WHERE id=?`, state, string(outcome), s.AttemptID); err != nil {
		return err
	}
	return saveRoot(ctx, tx, r)
}
func (l *Ledger) EndAttempt(ctx context.Context, s Scope, o Outcome) error {
	return l.transaction(ctx, "end attempt", func(tx *sql.Tx) error { return l.EndAttemptTx(ctx, tx, s, o) })
}

func (l *Ledger) ReconcileInterruptedTx(ctx context.Context, tx *sql.Tx) (err error) {
	defer func() { err = persist("reconcile interrupted", err) }()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT root_id FROM budget_attempts WHERE state='active'`)
	if err != nil {
		return err
	}
	var roots []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		roots = append(roots, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range roots {
		r, e := readRoot(ctx, tx, id)
		if e != nil {
			return e
		}
		if l.config.Now().UnixNano() < r.last {
			return ErrClock
		}
		var tail int64
		if e = tx.QueryRowContext(ctx, `SELECT MAX(lease_ns) FROM budget_attempts WHERE root_id=? AND state='active'`, id).Scan(&tail); e != nil {
			return e
		}
		if tail > r.last {
			r.Elapsed += time.Duration(tail - r.last)
		}
		r.last = max(r.last, l.config.Now().UnixNano())
		r.BlockedReason = "unknown"
		if _, e = tx.ExecContext(ctx, `UPDATE budget_reservations SET state='unknown' WHERE root_id=? AND state IN ('reserved','dispatched')`, id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE budget_attempts SET state='unknown',outcome='interrupted' WHERE root_id=? AND state='active'`, id); e != nil {
			return e
		}
		if e = saveRoot(ctx, tx, r); e != nil {
			return e
		}
	}
	return nil
}
func (l *Ledger) ReconcileInterrupted(ctx context.Context) error {
	return l.transaction(ctx, "reconcile interrupted", func(tx *sql.Tx) error { return l.ReconcileInterruptedTx(ctx, tx) })
}

func (l *Ledger) Snapshot(ctx context.Context, id string) (out Snapshot, err error) {
	err = l.transaction(ctx, "snapshot", func(tx *sql.Tx) error {
		r, e := readRoot(ctx, tx, id)
		if e != nil {
			return e
		}
		if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
			return e
		}
		if e = saveRoot(ctx, tx, r); e != nil {
			return e
		}
		out = r.Snapshot
		return nil
	})
	return
}

func readReservation(ctx context.Context, tx *sql.Tx, id string) (Reservation, string, error) {
	var r Reservation
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT id,root_id,member_id,session_id,attempt_id,fence,kind,digest,input_tokens,output_cap,held_tokens,state FROM budget_reservations WHERE id=?`, id).Scan(&r.OperationID, &r.Scope.RootBudgetID, &r.Scope.MemberID, &r.Scope.SessionID, &r.Scope.AttemptID, &r.Scope.Fence, &r.Kind, &digest, &r.InputTokens, &r.MaxOutputTokens, &r.HeldTokens, &r.State)
	return r, digest, err
}
func (l *Ledger) reserve(ctx context.Context, s Scope, kind, id, digest string, input int64, output int) (grant Reservation, err error) {
	if id == "" || digest == "" || input < 0 || output < 0 {
		return grant, ErrScope
	}
	// Bind the explicit request fields as well as the caller's content digest.
	digest = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", digest, input, output))))
	err = l.transaction(ctx, "reserve "+kind, func(tx *sql.Tx) error {
		old, oldDigest, e := readReservation(ctx, tx, id)
		if e == nil {
			if old.Scope != s || old.Kind != kind || oldDigest != digest {
				return ErrConflict
			}
			grant = old
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		r, e := readRoot(ctx, tx, s.RootBudgetID)
		if e != nil {
			return e
		}
		if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
			return e
		}
		if e = blocked(r); e != nil {
			return e
		}
		if e = l.checkEffect(ctx, tx, s); e != nil {
			return e
		}
		if kind == "model" {
			if r.Limits.MaxModelCalls > 0 && r.ModelCalls >= r.Limits.MaxModelCalls {
				return &LimitError{Resource: "model_calls"}
			}
			if cap := r.Limits.MaxOutputTokens; cap > 0 && (output == 0 || output > cap) {
				output = cap
			}
			if r.Limits.MaxTokens > 0 {
				remaining := r.Limits.MaxTokens - r.SpentTokens - r.HeldTokens - input
				if remaining <= 0 {
					return &LimitError{Resource: "tokens", Temporary: r.HeldTokens > 0 && r.Limits.MaxTokens-r.SpentTokens-input > 0}
				}
				if output == 0 {
					output = 4096
				}
				if int64(output) > remaining {
					output = int(remaining)
				}
			}
			if input > math.MaxInt64-int64(output) || r.HeldTokens > math.MaxInt64-input-int64(output) {
				return ErrScope
			}
			r.ModelCalls++
			r.HeldTokens += input + int64(output)
		} else {
			if r.Limits.MaxToolCalls > 0 && r.ToolCalls >= r.Limits.MaxToolCalls {
				return &LimitError{Resource: "tool_calls"}
			}
			r.ToolCalls++
		}
		grant = Reservation{OperationID: id, Scope: s, Kind: kind, State: "reserved", InputTokens: input, MaxOutputTokens: output, HeldTokens: input + int64(output)}
		if _, e = tx.ExecContext(ctx, `INSERT INTO budget_reservations(id,root_id,member_id,session_id,attempt_id,fence,kind,digest,input_tokens,output_cap,held_tokens,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,'reserved')`, id, s.RootBudgetID, s.MemberID, s.SessionID, s.AttemptID, s.Fence, kind, digest, input, output, grant.HeldTokens); e != nil {
			return e
		}
		return saveRoot(ctx, tx, r)
	})
	return
}
func (l *Ledger) ReserveModel(ctx context.Context, s Scope, r ModelRequest) (Reservation, error) {
	return l.reserve(ctx, s, "model", r.OperationID, r.Digest, r.InputTokens, r.MaxOutputTokens)
}
func (l *Ledger) ReserveTool(ctx context.Context, s Scope, r ToolRequest) (Reservation, error) {
	return l.reserve(ctx, s, "tool", r.OperationID, r.Digest, 0, 0)
}

// MarkDispatched returns true exactly once. False means the effect was already
// dispatched/settled, and never authorizes replay of external I/O.
func (l *Ledger) MarkDispatched(ctx context.Context, grant Reservation) (first bool, err error) {
	err = l.transaction(ctx, "dispatch", func(tx *sql.Tx) error {
		old, _, e := readReservation(ctx, tx, grant.OperationID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrScope
		}
		if e != nil {
			return e
		}
		if old.Scope != grant.Scope {
			return ErrScope
		}
		if old.State != "reserved" {
			if old.State == "unknown" {
				return ErrUnknown
			}
			if old.State == "paused" || old.State == "continued" {
				return ErrConflict
			}
			return nil
		}
		r, e := readRoot(ctx, tx, old.Scope.RootBudgetID)
		if e != nil {
			return e
		}
		if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
			return e
		}
		if e = blocked(r); e != nil {
			return e
		}
		if e = l.checkEffect(ctx, tx, old.Scope); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE budget_reservations SET state='dispatched' WHERE id=? AND state='reserved'`, old.OperationID); e != nil {
			return e
		}
		first = true
		return saveRoot(ctx, tx, r)
	})
	return
}

func (l *Ledger) Settle(ctx context.Context, grant Reservation, s Settlement) (usage harness.Usage, err error) {
	if s.Usage.InputTokens < 0 || s.Usage.OutputTokens < 0 || s.Usage.TotalTokens < 0 || s.Usage.InputTokens > math.MaxInt64-s.Usage.OutputTokens {
		return usage, ErrScope
	}
	raw, _ := json.Marshal(s)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	err = l.transaction(ctx, "settle", func(tx *sql.Tx) error {
		old, _, e := readReservation(ctx, tx, grant.OperationID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrScope
		}
		if e != nil {
			return e
		}
		if old.Scope != grant.Scope {
			return ErrScope
		}
		if e = checkOwner(ctx, tx, old.Scope, false); e != nil {
			return e
		}
		var previous string
		var stored []byte
		if e = tx.QueryRowContext(ctx, `SELECT settlement_digest,usage_json FROM budget_reservations WHERE id=?`, old.OperationID).Scan(&previous, &stored); e != nil {
			return e
		}
		if old.State == "settled" {
			if previous != digest {
				return ErrConflict
			}
			return json.Unmarshal(stored, &usage)
		}
		if old.State == "paused" || old.State == "continued" {
			return ErrConflict
		}
		r, e := readRoot(ctx, tx, old.Scope.RootBudgetID)
		if e != nil {
			return e
		}
		if e = l.advance(ctx, tx, r, l.config.Now().UnixNano()); e != nil {
			return e
		}
		if s.Unknown {
			r.BlockedReason = "unknown"
			if _, e = tx.ExecContext(ctx, `UPDATE budget_reservations SET state='unknown' WHERE id=?`, old.OperationID); e != nil {
				return e
			}
			return saveRoot(ctx, tx, r)
		}
		usage = s.Usage
		usage.TotalTokens = max(usage.TotalTokens, usage.InputTokens+usage.OutputTokens)
		if old.Kind == "tool" {
			usage = harness.Usage{}
		} else if !s.Complete {
			if usage.TotalTokens < old.HeldTokens {
				usage.TotalTokens = old.HeldTokens
				usage.Estimated = true
			}
		}
		if r.HeldTokens < old.HeldTokens || r.SpentTokens > math.MaxInt64-usage.TotalTokens {
			return ErrConflict
		}
		r.HeldTokens -= old.HeldTokens
		r.SpentTokens += usage.TotalTokens
		if r.Limits.MaxTokens > 0 && r.SpentTokens >= r.Limits.MaxTokens && r.BlockedReason == "" {
			r.BlockedReason = "tokens"
		}
		stored, e = json.Marshal(usage)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE budget_reservations SET state='settled',settlement_digest=?,usage_json=? WHERE id=?`, digest, stored, old.OperationID); e != nil {
			return e
		}
		return saveRoot(ctx, tx, r)
	})
	return
}
