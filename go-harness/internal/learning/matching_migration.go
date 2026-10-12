package learning

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func migrateMatching(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_learning_match_inputs(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,pattern_id TEXT NOT NULL,revision INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',job_id TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',
 UNIQUE(pattern_id,revision),FOREIGN KEY(pattern_id,revision) REFERENCES df_learning_pattern_revisions(pattern_id,revision) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS df_learning_match_input_pending ON df_learning_match_inputs(state,sequence);
INSERT OR IGNORE INTO df_learning_match_inputs(pattern_id,revision) SELECT id,current_revision FROM df_learning_patterns
 WHERE NOT EXISTS(SELECT 1 FROM df_learning_migrations WHERE name='skill-match-inputs-v1');
INSERT OR IGNORE INTO df_learning_migrations VALUES('skill-match-inputs-v1');
CREATE TABLE IF NOT EXISTS df_learning_match_scan(id INTEGER PRIMARY KEY CHECK(id=1),cursor INTEGER NOT NULL);
INSERT OR IGNORE INTO df_learning_match_scan VALUES(1,0);
CREATE TABLE IF NOT EXISTS df_learning_matches(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 pattern_id TEXT NOT NULL,revision INTEGER NOT NULL,job_id TEXT NOT NULL UNIQUE,
 policy_version TEXT NOT NULL,model_identity TEXT NOT NULL,access_identity TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,result BLOB,result_sha TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,
 FOREIGN KEY(pattern_id,revision) REFERENCES df_learning_pattern_revisions(pattern_id,revision) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS df_learning_match_session ON df_learning_matches(session_id,id);
CREATE TABLE IF NOT EXISTS df_learning_match_reassessments(
 pattern_id TEXT NOT NULL REFERENCES df_learning_patterns(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 2),
 previous_id TEXT NOT NULL UNIQUE REFERENCES df_learning_matches(id) ON DELETE CASCADE,
 fresh_id TEXT NOT NULL UNIQUE REFERENCES df_learning_matches(id) ON DELETE CASCADE,
 PRIMARY KEY(pattern_id,ordinal));
`)
	return err
}

func disableMatchingTx(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&exists); err != nil || !exists {
		return err
	}
	if err := skillmaintenance.Migrate(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE job_kind='skill_match' AND state IN ('queued','retry','running')`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='cancelled',reason='host_disabled'
 WHERE state='queued' AND job_id IN (SELECT id FROM df_skill_maintenance_jobs WHERE job_kind='skill_match' AND state='cancelled')`)
	return err
}
