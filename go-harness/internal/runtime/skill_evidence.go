package runtime

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

// The trigger covers every foreground/background terminal projection, including
// idle cancellation and native lease expiry. It runs in the lifecycle's own
// transaction, performs no model I/O, and never copies prompts/errors/results.
// Existing terminal runs are not backfilled: they have no trustworthy access
// observations. Pending/waiting/reconciliation states are not terminal evidence.
func migrateSkillEvidence(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_skill_reads (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 run_id TEXT NOT NULL REFERENCES harness_runs(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 root_id TEXT NOT NULL,attempt_id TEXT NOT NULL,role_id TEXT NOT NULL,
 source_id TEXT NOT NULL,source_key TEXT NOT NULL,scope TEXT NOT NULL,
 name TEXT NOT NULL,version TEXT NOT NULL,path TEXT NOT NULL,file_hash TEXT NOT NULL,
 bytes INTEGER NOT NULL,kind TEXT NOT NULL,observed_at TEXT NOT NULL,
 UNIQUE(run_id,attempt_id,role_id,source_key,name,version,path,kind));
CREATE INDEX IF NOT EXISTS harness_skill_reads_root ON harness_skill_reads(root_id,source_key,name,version);
CREATE INDEX IF NOT EXISTS harness_skill_reads_run ON harness_skill_reads(run_id,id);
CREATE TABLE IF NOT EXISTS harness_skill_outcomes (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 run_id TEXT UNIQUE NOT NULL REFERENCES harness_runs(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 status TEXT NOT NULL,effects_uncertain INTEGER NOT NULL,ended_at TEXT NOT NULL,
 processed INTEGER NOT NULL DEFAULT 0 CHECK(processed IN (0,1)));
CREATE INDEX IF NOT EXISTS harness_skill_outcomes_pending ON harness_skill_outcomes(processed,sequence);
CREATE TRIGGER IF NOT EXISTS harness_skill_terminal_outbox
AFTER UPDATE OF status ON harness_runs
WHEN NEW.status IN ('completed','failed','cancelled') AND OLD.status NOT IN ('completed','failed','cancelled')
BEGIN
 INSERT INTO harness_skill_outcomes(run_id,session_id,status,effects_uncertain,ended_at)
 VALUES(NEW.id,NEW.session_id,NEW.status,
 EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=NEW.id AND state IN ('started','uncertain')),NEW.updated_at);
END;`)
	return err
}

func evidenceHash(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// The caller must first fence its live foreground execution or background
// attempt in this transaction. Repeated reads in an attempt retain one event;
// distinct attempts and roles remain individually attributable.
func recordSkillReadTx(ctx context.Context, tx *sql.Tx, scope budget.Scope, evidence interaction.SkillRead) error {
	r := evidence.Read
	p := r.File.Path
	if evidence.RunID != scope.MemberID || evidence.SessionID != scope.SessionID || scope.RootBudgetID == "" || scope.AttemptID == "" ||
		!harness.ValidSkillName(r.Ref.SourceID) || !harness.ValidSkillName(r.Ref.Name) || !evidenceHash(r.Ref.SourceIdentity) || !evidenceHash(r.Ref.Version) || r.Ref.Hash != r.Ref.Version || !evidenceHash(r.File.Hash) ||
		(r.Scope != harness.SkillScopeWorkspace && r.Scope != harness.SkillScopeGlobal) || (r.Kind != "skill_load" && r.Kind != "support_read") ||
		(r.Kind == "skill_load" && p != "SKILL.md") || r.File.Bytes < 0 || len(p) == 0 || len(p) > 1024 || !utf8.ValidString(p) ||
		strings.ContainsAny(p, "\\:\x00") || strings.HasPrefix(p, "/") || p == "." || p == ".." || strings.HasPrefix(p, "../") || path.Clean(p) != p {
		return harness.ErrExecutionConflict
	}
	roleID := ""
	if evidence.Role != nil {
		if evidence.Role.RootID != scope.RootBudgetID {
			return harness.ErrExecutionConflict
		}
		saved, err := agents.ReadInvocation(ctx, tx, evidence.Role.ID, scope.RootBudgetID)
		if err != nil {
			return err
		}
		actual, err := json.Marshal(evidence.Role)
		if err != nil {
			return err
		}
		expected, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if string(actual) != string(expected) {
			return harness.ErrExecutionConflict
		}
		roleID = saved.ID
	}
	// Recheck pinned file identity against Registry storage; no directory access
	// or mutable installation pointer participates in attribution.
	var hash string
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT hash,size FROM df_skill_files WHERE source_key=? AND name=? AND version=? AND path=?`, r.Ref.SourceIdentity, r.Ref.Name, r.Ref.Version, p).Scan(&hash, &size); err != nil {
		return err
	}
	if hash != r.File.Hash || size != r.File.Bytes {
		return harness.ErrExecutionConflict
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_skill_reads(run_id,session_id,root_id,attempt_id,role_id,source_id,source_key,scope,name,version,path,file_hash,bytes,kind,observed_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,attempt_id,role_id,source_key,name,version,path,kind) DO NOTHING`, scope.MemberID, scope.SessionID, scope.RootBudgetID, scope.AttemptID, roleID,
		r.Ref.SourceID, r.Ref.SourceIdentity, r.Scope, r.Ref.Name, r.Ref.Version, p, r.File.Hash, r.File.Bytes, r.Kind, timestamp())
	return err
}

func (a *executionAttempt) RecordSkillRead(ctx context.Context, evidence interaction.SkillRead) error {
	if err := a.service.Coordinator.Authorize(a.lease.Scope.SessionID, a.lease.OwnerID); err != nil {
		return err
	}
	return withExecutionTransaction(ctx, a.service.Store, func(tx *sql.Tx) error {
		if _, err := checkExecutionLease(ctx, tx, a.lease); err != nil {
			return err
		}
		return recordSkillReadTx(ctx, tx, a.lease.Scope, evidence)
	})
}

func (a *BackgroundInteractionAttempt) RecordSkillRead(ctx context.Context, evidence interaction.SkillRead) error {
	return withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		if evidence.Role == nil {
			// Legacy saved generic tasks predate role registration. Their fenced
			// run/task and root remain sufficient access provenance; do not invent
			// a role identity or break otherwise resumable historical tasks.
			if a.scope.Binding.AgentVersion != "deerflow-child-v1" {
				return harness.ErrExecutionConflict
			}
			return recordSkillReadTx(ctx, tx, background.BudgetScope(a.scope), evidence)
		}
		allowed, err := agents.IsDescendant(ctx, tx, a.scope.Binding.RootBudgetID, a.scope.Binding.RoleID(), evidence.Role.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return harness.ErrPermissionDenied
		}
		return recordSkillReadTx(ctx, tx, background.BudgetScope(a.scope), evidence)
	})
}
