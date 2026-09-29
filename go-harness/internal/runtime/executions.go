package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

// ExecutionLease is host-owned, tied to one connection generation and one
// budget attempt. The owning coordinator lease must surround every operation.
type ExecutionLease struct {
	Scope   budget.Scope
	OwnerID string
	InputID string
}

type executionRow struct {
	State        harness.ExecutionState
	RootBudgetID string
	Config       harness.Session
	InputDigest  string
	Manifest     *ExecutionManifest
}

func migrateExecutions(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_executions (
 run_id TEXT PRIMARY KEY REFERENCES harness_runs(id),session_id TEXT NOT NULL REFERENCES harness_sessions(id),input_id TEXT NOT NULL REFERENCES harness_inputs(id),
 status TEXT NOT NULL,version INTEGER NOT NULL,attempt_id TEXT NOT NULL,attempt INTEGER NOT NULL,root_budget_id TEXT NOT NULL,
 config BLOB NOT NULL,input_digest TEXT NOT NULL,manifest BLOB,blocked_reason TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS harness_execution_live_session ON harness_executions(session_id) WHERE status IN ('running','suspending','waiting_input','resuming');
CREATE TABLE IF NOT EXISTS harness_execution_attempts (
 id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES harness_executions(run_id),ordinal INTEGER NOT NULL,owner_id TEXT NOT NULL,
 status TEXT NOT NULL,base_checkpoint_sha TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,finished_at TEXT NOT NULL DEFAULT '',UNIQUE(run_id,ordinal));
CREATE TABLE IF NOT EXISTS harness_execution_intents (
 id TEXT PRIMARY KEY,run_id TEXT NOT NULL,tool_call_id TEXT NOT NULL,version INTEGER NOT NULL,state TEXT NOT NULL,request BLOB NOT NULL,descriptor BLOB NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 UNIQUE(run_id,tool_call_id),FOREIGN KEY(run_id) REFERENCES harness_executions(run_id),FOREIGN KEY(run_id,tool_call_id) REFERENCES harness_tool_receipts(run_id,tool_call_id));
CREATE TABLE IF NOT EXISTS harness_execution_grants (
 id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES harness_executions(run_id),attempt_id TEXT NOT NULL REFERENCES harness_execution_attempts(id),intent_id TEXT NOT NULL REFERENCES harness_execution_intents(id),
 intent_version INTEGER NOT NULL,decision TEXT NOT NULL,state TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(attempt_id,intent_id));
CREATE TABLE IF NOT EXISTS harness_execution_audits (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES harness_executions(run_id),attempt_id TEXT NOT NULL,kind TEXT NOT NULL,data BLOB NOT NULL,created_at TEXT NOT NULL);
`)
	return err
}

type executionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readExecution(ctx context.Context, q executionQuery, sessionID, runID string) (executionRow, error) {
	var row executionRow
	var config, manifest []byte
	var created, updated string
	err := q.QueryRowContext(ctx, `SELECT session_id,run_id,input_id,status,version,attempt_id,attempt,root_budget_id,config,input_digest,manifest,blocked_reason,created_at,updated_at FROM harness_executions WHERE session_id=? AND run_id=?`, sessionID, runID).Scan(
		&row.State.SessionID, &row.State.RunID, &row.State.InputID, &row.State.Status, &row.State.Version, &row.State.AttemptID, &row.State.Attempt, &row.RootBudgetID, &config, &row.InputDigest, &manifest, &row.State.BlockedReason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, harness.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	if err = json.Unmarshal(config, &row.Config); err != nil {
		return row, err
	}
	if len(manifest) > 0 {
		row.Manifest = &ExecutionManifest{}
		if err = json.Unmarshal(manifest, row.Manifest); err != nil {
			return row, err
		}
		row.State.EventCursor = row.Manifest.EventCursor
	}
	row.State.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return row, err
	}
	row.State.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return row, err
}

func executionConfigDigest(x harness.Session) string {
	data, _ := json.Marshal(struct {
		ID, CWD, Model, Mode, Approval string
		Subagents                      bool
		Version                        int64
	}{x.ID, x.CWD, x.Model, x.Mode, x.ApprovalMode, x.Subagents, x.ConfigVersion})
	return executionDigest(data)
}
func executionDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func executionInvalid(message string) error {
	return fmt.Errorf("%w: %s", harness.ErrInvalidInput, message)
}

func executionCAS(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return harness.ErrExecutionConflict
	}
	return nil
}

// BeginExecutionTx joins the accepted run/input and ledger BeginAttemptTx in
// the caller's transaction. It neither creates an input nor opens a transaction.
func (s *Store) BeginExecutionTx(ctx context.Context, tx *sql.Tx, req harness.RunRequest, owner string, scope budget.Scope) (ExecutionLease, error) {
	lease := ExecutionLease{Scope: scope, OwnerID: owner, InputID: req.InputID}
	if owner == "" || req.Session.ID == "" || req.RunID == "" || req.InputID == "" || scope.RootBudgetID == "" || req.RootBudgetID != scope.RootBudgetID || scope.MemberID != req.RunID || scope.SessionID != req.Session.ID || scope.AttemptID == "" || scope.Fence != 1 {
		return lease, executionInvalid("invalid initial execution identity")
	}
	var sid, inputID, status string
	var savedInput []byte
	err := tx.QueryRowContext(ctx, `SELECT r.session_id,r.input_id,r.status,i.content FROM harness_runs r JOIN harness_inputs i ON i.id=r.input_id WHERE r.id=?`, req.RunID).Scan(&sid, &inputID, &status, &savedInput)
	if err != nil {
		return lease, err
	}
	input, err := json.Marshal(req.Input)
	if err != nil {
		return lease, err
	}
	if sid != req.Session.ID || inputID != req.InputID || status != "running" || executionDigest(savedInput) != executionDigest(input) {
		return lease, harness.ErrExecutionConflict
	}
	current, err := readExecutionSession(ctx, tx, sid)
	if err != nil {
		return lease, err
	}
	if executionConfigDigest(current) != executionConfigDigest(req.Session) {
		return lease, harness.ErrExecutionConflict
	}
	var occupied bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_executions WHERE session_id=? AND status IN ('running','suspending','waiting_input','resuming'))`, sid).Scan(&occupied); err != nil {
		return lease, err
	}
	if occupied {
		return lease, harness.ErrExecutionWaitingInput
	}
	config, err := json.Marshal(req.Session)
	if err != nil {
		return lease, err
	}
	now := timestamp()
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_executions(run_id,session_id,input_id,status,version,attempt_id,attempt,root_budget_id,config,input_digest,created_at,updated_at) VALUES(?,?,?,'running',1,?,1,?,?,?,?,?)`, req.RunID, sid, inputID, scope.AttemptID, scope.RootBudgetID, config, executionDigest(input), now, now)
	if err != nil {
		return lease, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_attempts(id,run_id,ordinal,owner_id,status,created_at) VALUES(?,?,1,?,'running',?)`, scope.AttemptID, req.RunID, owner, now)
	if err != nil {
		return lease, err
	}
	return lease, executionAudit(ctx, tx, lease, "attempt_started", map[string]any{"attempt": int64(1)})
}

func readExecutionSession(ctx context.Context, q executionQuery, id string) (harness.Session, error) {
	return scanSession(q.QueryRowContext(ctx, `SELECT s.id,s.cwd,s.title,s.mode,s.model,s.created_at,s.updated_at,COALESCE(c.approval_mode,'ask'),COALESCE(c.subagents,1),COALESCE(c.version,1) FROM harness_sessions s LEFT JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?`, id))
}
func executionAudit(ctx context.Context, tx *sql.Tx, lease ExecutionLease, kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_execution_audits(run_id,attempt_id,kind,data,created_at) VALUES(?,?,?,?,?)`, lease.Scope.MemberID, lease.Scope.AttemptID, kind, data, timestamp())
	return err
}
func checkExecutionLease(ctx context.Context, tx *sql.Tx, lease ExecutionLease) (executionRow, error) {
	row, err := readExecution(ctx, tx, lease.Scope.SessionID, lease.Scope.MemberID)
	if err != nil {
		return row, err
	}
	var owner, status string
	err = tx.QueryRowContext(ctx, `SELECT owner_id,status FROM harness_execution_attempts WHERE id=? AND run_id=? AND ordinal=?`, lease.Scope.AttemptID, lease.Scope.MemberID, lease.Scope.Fence).Scan(&owner, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return row, harness.ErrExecutionConflict
	}
	if err != nil {
		return row, err
	}
	if lease.OwnerID == "" || owner != lease.OwnerID || status != "running" || row.State.AttemptID != lease.Scope.AttemptID || row.State.Attempt != lease.Scope.Fence || row.RootBudgetID != lease.Scope.RootBudgetID || row.State.InputID != lease.InputID {
		return row, harness.ErrExecutionConflict
	}
	switch row.State.Status {
	case harness.ExecutionRunning, harness.ExecutionResuming, harness.ExecutionSuspending:
	default:
		return row, harness.ErrExecutionConflict
	}
	return row, nil
}

// CheckExecutionEffectTx is suitable for a ledger fencing hook. It checks the
// host-created scope, never a scope decoded from an ACP request.
func (s *Store) CheckExecutionEffectTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
	row, err := readExecution(ctx, tx, scope.SessionID, scope.MemberID)
	if err != nil {
		return err
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner_id FROM harness_execution_attempts WHERE id=?`, scope.AttemptID).Scan(&owner); err != nil {
		return err
	}
	_, err = checkExecutionLease(ctx, tx, ExecutionLease{Scope: scope, OwnerID: owner, InputID: row.State.InputID})
	return err
}

func (s *Store) requireNoWaitingExecution(ctx context.Context, sessionID string) error {
	var blocked bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_executions WHERE session_id=? AND status IN ('suspending','waiting_input','resuming'))`, sessionID).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return harness.ErrExecutionWaitingInput
	}
	return nil
}

// Execution reads a business projection. The public service must acquire the
// owner's coordinator lease before calling it; this store does not authorize.
func (s *Store) Execution(ctx context.Context, sessionID, runID string) (harness.ExecutionState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.ExecutionState{}, err
	}
	defer tx.Rollback()
	if runID == "" {
		err = tx.QueryRowContext(ctx, `SELECT run_id FROM harness_executions WHERE session_id=? ORDER BY created_at DESC,run_id DESC LIMIT 1`, sessionID).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			return harness.ExecutionState{}, harness.ErrNotFound
		}
		if err != nil {
			return harness.ExecutionState{}, err
		}
	}
	row, err := readExecution(ctx, tx, sessionID, runID)
	if err != nil {
		return row.State, err
	}
	row.State.WaitingInputs, err = executionInteractions(ctx, tx, runID)
	if err != nil {
		return row.State, err
	}
	row.State.EventCursor, err = executionEventCursor(ctx, tx, sessionID)
	if err != nil {
		return row.State, err
	}
	if row.State.Status == harness.ExecutionWaitingInput {
		if err = validateExecutionManifest(ctx, tx, row); err == nil {
			row.State.Resumable = true
		} else if errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrReconciliationRequired) || errors.Is(err, harness.ErrExecutionConflict) {
			row.State.BlockedReason = err.Error()
		} else {
			return row.State, err
		}
	}
	return row.State, nil
}

func validateExecutionConfig(ctx context.Context, tx *sql.Tx, row executionRow) error {
	x, err := readExecutionSession(ctx, tx, row.State.SessionID)
	if err != nil {
		return err
	}
	if !session.SameWorkspace(x.CWD, row.Config.CWD) || executionConfigDigest(x) != executionConfigDigest(row.Config) {
		return fmt.Errorf("%w: session configuration changed", harness.ErrExecutionUnresumable)
	}
	var input []byte
	if err = tx.QueryRowContext(ctx, `SELECT content FROM harness_inputs WHERE id=? AND run_id=?`, row.State.InputID, row.State.RunID).Scan(&input); err != nil {
		return err
	}
	if executionDigest(input) != row.InputDigest {
		return fmt.Errorf("%w: accepted input changed", harness.ErrExecutionUnresumable)
	}
	return nil
}
