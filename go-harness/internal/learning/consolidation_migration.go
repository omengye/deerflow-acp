package learning

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func migrateConsolidation(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_learning_consolidation_inputs(
 experience_id TEXT PRIMARY KEY REFERENCES df_learning_experiences(id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'pending',job_id TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS df_learning_consolidation_input_pending ON df_learning_consolidation_inputs(state,experience_id);
CREATE TABLE IF NOT EXISTS df_learning_migrations(name TEXT PRIMARY KEY);
INSERT OR IGNORE INTO df_learning_consolidation_inputs(experience_id) SELECT id FROM df_learning_experiences
 WHERE NOT EXISTS(SELECT 1 FROM df_learning_migrations WHERE name='consolidation-inputs-v1');
INSERT OR IGNORE INTO df_learning_migrations VALUES('consolidation-inputs-v1');
CREATE TABLE IF NOT EXISTS df_learning_consolidation_scan(id INTEGER PRIMARY KEY CHECK(id=1),cursor TEXT NOT NULL);
INSERT OR IGNORE INTO df_learning_consolidation_scan VALUES(1,'');
CREATE TABLE IF NOT EXISTS df_learning_consolidations(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 experience_id TEXT NOT NULL REFERENCES df_learning_experiences(id) ON DELETE CASCADE,
 job_id TEXT NOT NULL UNIQUE,policy_version TEXT NOT NULL,model_identity TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,result BLOB,result_sha TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_consolidation_session ON df_learning_consolidations(session_id,job_id);
CREATE TABLE IF NOT EXISTS df_learning_patterns(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 owner_key TEXT NOT NULL,current_revision INTEGER NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_pattern_session ON df_learning_patterns(session_id,updated_at,id);
CREATE TABLE IF NOT EXISTS df_learning_pattern_revisions(
 pattern_id TEXT NOT NULL REFERENCES df_learning_patterns(id) ON DELETE CASCADE,revision INTEGER NOT NULL,
 consolidation_id TEXT NOT NULL REFERENCES df_learning_consolidations(id) ON DELETE CASCADE,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,PRIMARY KEY(pattern_id,revision));
CREATE TABLE IF NOT EXISTS df_learning_pattern_members(
 pattern_id TEXT NOT NULL,revision INTEGER NOT NULL,
 experience_id TEXT NOT NULL REFERENCES df_learning_experiences(id) ON DELETE CASCADE,
 experience_sha TEXT NOT NULL,episode_id TEXT NOT NULL,independence_key TEXT NOT NULL,epoch INTEGER NOT NULL,
 PRIMARY KEY(pattern_id,revision,experience_id),
 FOREIGN KEY(pattern_id,revision) REFERENCES df_learning_pattern_revisions(pattern_id,revision) ON DELETE CASCADE);
`)
	return err
}

func disableConsolidationTx(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&exists); err != nil || !exists {
		return err
	}
	if err := skillmaintenance.Migrate(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE job_kind='experience_consolidate' AND state IN ('queued','retry','running')`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='cancelled',reason='host_disabled'
 WHERE state='queued' AND job_id IN (SELECT id FROM df_skill_maintenance_jobs WHERE job_kind='experience_consolidate' AND state='cancelled')`)
	return err
}
