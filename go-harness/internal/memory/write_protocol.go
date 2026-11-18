package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const writeMigration = `
ALTER TABLE memory_fact_revisions ADD COLUMN provenance TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE memory_staged_facts ADD COLUMN provenance TEXT NOT NULL DEFAULT 'unknown';
UPDATE memory_fact_revisions SET provenance='external_context' WHERE policy_version='personal-explicit-external-context-v1';
CREATE TABLE memory_extraction_results(
 id TEXT PRIMARY KEY,run_id TEXT NOT NULL,input_id TEXT NOT NULL,policy TEXT NOT NULL,schema_version INTEGER NOT NULL,
 raw TEXT NOT NULL,sha TEXT NOT NULL,provenance TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE memory_extraction_items(
 result_id TEXT NOT NULL REFERENCES memory_extraction_results(id),ordinal INTEGER NOT NULL,
 status TEXT NOT NULL,reason TEXT NOT NULL,operation_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(result_id,ordinal));
CREATE TABLE memory_daily_attempts(day TEXT PRIMARY KEY,attempts INTEGER NOT NULL);
INSERT INTO memory_daily_attempts(day,attempts)
 SELECT strftime('%Y-%m-%d','now'),COALESCE(SUM(attempts),0) FROM memory_jobs WHERE updated_at>=strftime('%Y-%m-%d','now');
`

// OptionalWriteFailure proves that an isolated optional transaction did not
// commit. Commit/rollback ambiguity is deliberately not wrapped.
type OptionalWriteFailure struct{ cause error }

func (e *OptionalWriteFailure) Error() string { return "optional memory transaction rolled back" }
func (e *OptionalWriteFailure) Unwrap() error { return e.cause }

func optionalTransact(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		if rollback := tx.Rollback(); rollback != nil {
			return errors.Join(err, rollback)
		}
		return &OptionalWriteFailure{err}
	}
	return tx.Commit()
}

func (s *Store) RecordDecision(ctx context.Context, runID, inputID, decision string) error {
	return optionalTransact(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE memory_inputs SET decision=? WHERE run_id=? AND input_id=? AND job_id='' AND decision IN ('pending','started','staged')", decision, runID, inputID)
		return err
	})
}

// The fallback contains identifiers and fixed reason codes, never model text,
// SQL error messages or secrets. A failed audit never recursively audits itself.
func (s *Store) AuditFallback(runID, inputID, reason string) {
	s.auditFailures.Add(1)
	slog.Warn("optional memory extraction audit unavailable", "run_id", runID, "input_id", inputID, "reason", reason)
}
func (s *Store) AuditFailureCount() uint64 { return s.auditFailures.Load() }

func saveExtractionResultTx(ctx context.Context, tx *sql.Tx, id, runID, inputID, raw, provenance string) error {
	if _, err := extractionEnvelope([]byte(raw)); err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	var previous, policy, storedRun, storedInput, storedProvenance string
	var version int
	err := tx.QueryRowContext(ctx, "SELECT sha,policy,run_id,input_id,schema_version,provenance FROM memory_extraction_results WHERE id=?", id).Scan(&previous, &policy, &storedRun, &storedInput, &version, &storedProvenance)
	if err == nil {
		if previous != digest || policy != WritePolicy || storedRun != runID || storedInput != inputID || version != ExtractionSchemaVersion || storedProvenance != provenance {
			return ErrVersionConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO memory_extraction_results(id,run_id,input_id,policy,schema_version,raw,sha,provenance,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
		id, runID, inputID, WritePolicy, ExtractionSchemaVersion, raw, digest, provenance, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ExtractionResult(ctx context.Context, runID, inputID string) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT raw FROM memory_extraction_results WHERE id=? AND run_id=? AND input_id=? AND policy=?", "input/"+inputID, runID, inputID, WritePolicy).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func (s *Store) ExtractionProvenance(ctx context.Context, runID, inputID string) (string, error) {
	var provenance string
	err := s.db.QueryRowContext(ctx, "SELECT provenance FROM memory_extraction_results WHERE id=? AND run_id=? AND input_id=? AND policy=?", "input/"+inputID, runID, inputID, WritePolicy).Scan(&provenance)
	return provenance, err
}
func (s *Store) SaveExtractionResult(ctx context.Context, runID, inputID, raw, provenance string) error {
	return optionalTransact(ctx, s.db, func(tx *sql.Tx) error {
		return saveExtractionResultTx(ctx, tx, "input/"+inputID, runID, inputID, raw, provenance)
	})
}

func recordItemsTx(ctx context.Context, tx *sql.Tx, id string, rejected []Rejection) error {
	for _, r := range rejected {
		if _, err := tx.ExecContext(ctx, "INSERT INTO memory_extraction_items(result_id,ordinal,status,reason) VALUES(?,?,'rejected',?) ON CONFLICT(result_id,ordinal) DO NOTHING", id, r.Ordinal, r.Reason); err != nil {
			return err
		}
	}
	return nil
}

func contextProvenance(ctx context.Context, q sourceQuery, m InputSnapshot) (string, error) {
	if m.ContextExternal {
		return ExternalContextProvenance, nil
	}
	var tools bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=?)", m.SessionID).Scan(&tools); err != nil {
		return "", err
	}
	if tools {
		return ExternalContextProvenance, nil
	}
	return UserSupportedProvenance, nil
}
func (s *Store) ContextProvenance(ctx context.Context, m InputSnapshot) (string, error) {
	return contextProvenance(ctx, s.db, m)
}
func (s *Store) InputDecision(ctx context.Context, runID, inputID string) (string, error) {
	var decision string
	err := s.db.QueryRowContext(ctx, "SELECT decision FROM memory_inputs WHERE run_id=? AND input_id=?", runID, inputID).Scan(&decision)
	return decision, err
}

// SetDailyAttempts is configured once before starting the worker.
func (s *Store) SetDailyAttempts(limit int) error {
	if limit < 0 || limit > 1000000 {
		return harness.ErrInvalidInput
	}
	s.dailyAttempts = limit
	return nil
}

// ForgetTarget is for host-side validation only; deleted text is never returned
// by the model-facing state tool. The subsequent mutation still enforces CAS.
func (s *Store) ForgetTarget(ctx context.Context, scope Scope, key, id string) (Fact, string, error) {
	if (key == "") == (id == "") {
		return Fact{}, "", harness.ErrInvalidInput
	}
	key = CanonicalKey(key)
	var f Fact
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		if id == "" {
			var err error
			f, _, err = slotHead(ctx, tx, scope, key)
			return err
		}
		var err error
		f, err = scanFact(tx.QueryRowContext(ctx, "SELECT "+factColumns+" FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.id=? AND f.scope_key=?", id, scope.key))
		if errors.Is(err, harness.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT slot_key FROM memory_slots WHERE fact_id=? AND scope_key=?", id, scope.key).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return f, key, err
}
