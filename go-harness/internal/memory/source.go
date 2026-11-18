package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const SourceProtocol = "memory-source-v1"

var ErrSourceSnapshotMissing = errors.New("legacy_source_snapshot_missing")
var ErrSourceSuperseded = errors.New("memory source superseded by deletion")
var ErrSourcePolicyMissing = errors.New("legacy_memory_policy_unverifiable")

// This journal is separate from the operator-reviewed data-import journal.
const sourceMigration = `
CREATE TABLE memory_scope_barriers(scope_key TEXT PRIMARY KEY, epoch INTEGER NOT NULL);
CREATE TABLE memory_inputs(
 input_id TEXT PRIMARY KEY, run_id TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL,
 manifest BLOB NOT NULL, decision TEXT NOT NULL, job_id TEXT NOT NULL DEFAULT '');
CREATE INDEX memory_inputs_session ON memory_inputs(session_id,input_id);
CREATE TABLE memory_operation_sources(
 operation_id TEXT PRIMARY KEY REFERENCES memory_operations(operation_id),
 run_id TEXT NOT NULL, input_id TEXT NOT NULL, action TEXT NOT NULL,
 target_kind TEXT NOT NULL, target_id TEXT NOT NULL);
CREATE INDEX memory_operation_sources_input ON memory_operation_sources(run_id,input_id);
CREATE TABLE memory_job_dispositions(
 job_id TEXT PRIMARY KEY, reason TEXT NOT NULL, usage_state TEXT NOT NULL);
`

func (s *Store) initSourceProtocol(ctx context.Context) error {
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_schema_migrations(
 version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM memory_schema_migrations").Scan(&version); err != nil {
			return err
		}
		migrations := []struct{ name, sql string }{
			{"memory-source-delete-v1", sourceMigration},
			{"memory-write-policy-v2", writeMigration},
			{"memory-job-budget-v1", jobBudgetMigration},
			{"memory-index-retry-v1", indexMigration},
			{"memory-relearn-v1", relearnMigration},
		}
		if version > len(migrations) {
			return fmt.Errorf("%w: unsupported memory schema version %d", harness.ErrInvalidInput, version)
		}
		for i, m := range migrations {
			checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(m.sql)))
			if i+1 <= version {
				var name, old string
				if err := tx.QueryRowContext(ctx, "SELECT name,checksum FROM memory_schema_migrations WHERE version=?", i+1).Scan(&name, &old); err != nil {
					return err
				}
				if name != m.name || old != checksum {
					return fmt.Errorf("%w: memory migration identity changed", harness.ErrInvalidInput)
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO memory_schema_migrations(version,name,checksum,applied_at) VALUES(?,?,?,?)", i+1, m.name, checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		return nil
	})
}

type SourceScope struct {
	Key         string           `json:"key"`
	Revision    int64            `json:"revision"`
	DeleteEpoch int64            `json:"delete_epoch"`
	Slots       map[string]int64 `json:"slots"`
}

// InputSnapshot is immutable accepted-source data, not a checkpoint projection.
// Decision/job state lives in separate columns so retries cannot rebase it.
type InputSnapshot struct {
	RelearnPolicy     string                    `json:"relearn_policy,omitempty"`
	Protocol          string                    `json:"protocol"`
	PolicyVersion     string                    `json:"policy_version"`
	WritePolicy       string                    `json:"write_policy,omitempty"`
	ContextExternal   bool                      `json:"context_external,omitempty"`
	RunID             string                    `json:"run_id"`
	InputID           string                    `json:"input_id"`
	SessionID         string                    `json:"session_id"`
	ProfileID         string                    `json:"profile_id"`
	Workspace         string                    `json:"workspace"`
	RootBudgetID      string                    `json:"root_budget_id"`
	InputSHA          string                    `json:"input_sha"`
	Sequence          int64                     `json:"sequence"`
	Text              string                    `json:"-"`
	Writable          bool                      `json:"writable"`
	AutomaticEligible bool                      `json:"automatic_eligible"`
	Scopes            map[ScopeKind]SourceScope `json:"scopes"`
}

// CaptureInputTx must only be called by the host's human-input acceptance path,
// in the transaction inserting harness_inputs and its original user event.
func (s *Store) CaptureInputTx(ctx context.Context, tx *sql.Tx, req harness.RunRequest, sequence int64, input []byte, profileID string, scopes []Scope, writable, automatic bool) error {
	if req.InputID == "" || req.RunID == "" || req.Session.ID == "" || sequence < 1 ||
		(req.RootBudgetID != "" && req.RootBudgetID != req.RunID) {
		return harness.ErrInvalidInput
	}
	m := InputSnapshot{Protocol: SourceProtocol, PolicyVersion: "memory-source-delete-v1", WritePolicy: WritePolicy, RunID: req.RunID, InputID: req.InputID,
		RelearnPolicy: RelearnPolicy,
		SessionID:     req.Session.ID, ProfileID: profileID, Workspace: req.Session.CWD,
		RootBudgetID: req.RootBudgetID, InputSHA: fmt.Sprintf("%x", sha256.Sum256(input)),
		Sequence: sequence, Writable: writable, AutomaticEligible: writable && automatic,
		Scopes: map[ScopeKind]SourceScope{}}
	var text strings.Builder
	for _, part := range req.Input {
		if part.Type == "text" {
			text.WriteString(part.Text)
			text.WriteByte('\n')
		}
	}
	m.Text = text.String()
	decision := "pending"
	if !m.AutomaticEligible {
		decision = "disabled"
	} else if strings.TrimSpace(m.Text) == "" || len(m.Text) > 8192 {
		m.AutomaticEligible, decision = false, "source_text_ineligible"
	}
	for _, scope := range scopes {
		version, err := checkScope(ctx, tx, scope, true)
		if err != nil {
			return err
		}
		epoch, err := deletionEpochTx(ctx, tx, scope.key)
		if err != nil {
			return err
		}
		slots, err := slotVersions(ctx, tx, scope)
		if err != nil {
			return err
		}
		m.Scopes[scope.kind] = SourceScope{Key: scope.key, Revision: version, DeleteEpoch: epoch, Slots: slots}
		var visible bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_facts WHERE scope_key=? AND deleted=0)", scope.key).Scan(&visible); err != nil {
			return err
		}
		m.ContextExternal = m.ContextExternal || visible
	}
	var history bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND id<>?)", req.Session.ID, req.RunID).Scan(&history); err != nil {
		return err
	}
	m.ContextExternal = m.ContextExternal || history
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Deliberately no upsert: accepting an input twice is a host error.
	_, err = tx.ExecContext(ctx, "INSERT INTO memory_inputs(input_id,run_id,session_id,manifest,decision) VALUES(?,?,?,?,?)",
		m.InputID, m.RunID, m.SessionID, raw, decision)
	return err
}

type sourceQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func inputSnapshot(ctx context.Context, q sourceQuery, runID, inputID string) (InputSnapshot, error) {
	var m InputSnapshot
	var raw []byte
	err := q.QueryRowContext(ctx, "SELECT manifest FROM memory_inputs WHERE run_id=? AND input_id=?", runID, inputID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrSourceSnapshotMissing
	}
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Protocol != SourceProtocol || m.PolicyVersion != "memory-source-delete-v1" || m.RunID != runID || m.InputID != inputID || m.Sequence < 1 {
		return m, harness.ErrInvalidInput
	}
	// Keep only the digest and original event reference in the manifest. Never
	// duplicate user text into a second retention domain or trust job payloads.
	var input []byte
	var sessionID string
	if err = q.QueryRowContext(ctx, `SELECT i.content,r.session_id FROM harness_inputs i
 JOIN harness_runs r ON r.id=i.run_id WHERE i.id=? AND r.id=? AND r.input_id=i.id`, inputID, runID).Scan(&input, &sessionID); err != nil {
		return m, err
	}
	if sessionID != m.SessionID || fmt.Sprintf("%x", sha256.Sum256(input)) != m.InputSHA {
		return m, harness.ErrInvalidInput
	}
	var eventRaw []byte
	if err = q.QueryRowContext(ctx, "SELECT event FROM harness_events WHERE sequence=? AND run_id=?", m.Sequence, runID).Scan(&eventRaw); err != nil {
		return m, err
	}
	var event harness.RunEvent
	if err = json.Unmarshal(eventRaw, &event); err != nil {
		return m, err
	}
	if event.Kind != "user_message" || event.SessionID != m.SessionID || event.RunID != m.RunID {
		return m, harness.ErrPermissionDenied
	}
	var parts []harness.Content
	if err = json.Unmarshal(input, &parts); err != nil {
		return m, err
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			text.WriteString(part.Text)
			text.WriteByte('\n')
		}
	}
	m.Text = text.String()
	return m, nil
}

func (s *Store) InputSnapshot(ctx context.Context, runID, inputID string) (InputSnapshot, error) {
	return inputSnapshot(ctx, s.db, runID, inputID)
}

func deletionEpochTx(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, "SELECT epoch FROM memory_scope_barriers WHERE scope_key=?", key).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return epoch, err
}

func bumpDeletionEpochTx(ctx context.Context, tx *sql.Tx, scope Scope) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_scope_barriers(scope_key,epoch) VALUES(?,1)
 ON CONFLICT(scope_key) DO UPDATE SET epoch=epoch+1`, scope.key)
	return err
}

func sourceScopeTx(ctx context.Context, tx *sql.Tx, scope Scope, source Source, revocationOnly bool) (SourceScope, error) {
	m, err := inputSnapshot(ctx, tx, source.RunID, source.InputID)
	if err != nil {
		return SourceScope{}, err
	}
	if m.WritePolicy != WritePolicy {
		return SourceScope{}, ErrSourcePolicyMissing
	}
	pinned, ok := m.Scopes[scope.kind]
	if !m.Writable || !ok || pinned.Key != scope.key || source.EventSequence != m.Sequence {
		return SourceScope{}, harness.ErrPermissionDenied
	}
	epoch, err := deletionEpochTx(ctx, tx, scope.key)
	if err != nil {
		return SourceScope{}, err
	}
	if epoch != pinned.DeleteEpoch && !revocationOnly {
		return SourceScope{}, ErrSourceSuperseded
	}
	return pinned, nil
}

func explicitActionTx(ctx context.Context, tx *sql.Tx, runID, inputID string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_operation_sources
 WHERE run_id=? AND input_id=?)`, runID, inputID).Scan(&found)
	return found, err
}

func setInputDecisionTx(ctx context.Context, tx *sql.Tx, runID, inputID, decision string) error {
	_, err := tx.ExecContext(ctx, "UPDATE memory_inputs SET decision=? WHERE run_id=? AND input_id=? AND job_id=''", decision, runID, inputID)
	return err
}
