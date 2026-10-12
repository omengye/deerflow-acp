package learning

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func migrateWorkspaceLearning(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_learning_sharing(
 session_id TEXT PRIMARY KEY REFERENCES harness_sessions(id) ON DELETE CASCADE,
 workspace_shared INTEGER NOT NULL CHECK(workspace_shared IN (0,1)));
CREATE TABLE IF NOT EXISTS df_learning_episode_lineage(
 episode_id TEXT PRIMARY KEY REFERENCES df_learning_episodes(id) ON DELETE CASCADE,
 independence_key TEXT NOT NULL,origin TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_lineage_key ON df_learning_episode_lineage(independence_key);
INSERT OR IGNORE INTO df_learning_episode_lineage SELECT id,independence_key,'legacy_task' FROM df_learning_episodes
 WHERE NOT EXISTS(SELECT 1 FROM df_learning_migrations WHERE name='episode-lineage-v1');
INSERT OR IGNORE INTO df_learning_migrations VALUES('episode-lineage-v1');
CREATE TABLE IF NOT EXISTS df_learning_pattern_sharing(
 pattern_id TEXT PRIMARY KEY REFERENCES df_learning_patterns(id) ON DELETE CASCADE,workspace_key TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_pattern_workspace ON df_learning_pattern_sharing(workspace_key,pattern_id);
CREATE TABLE IF NOT EXISTS df_learning_consolidation_sharing(
 job_id TEXT PRIMARY KEY REFERENCES df_learning_consolidations(job_id) ON DELETE CASCADE,workspace_key TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_learning_consolidation_sources(
 job_id TEXT NOT NULL REFERENCES df_learning_consolidations(job_id) ON DELETE CASCADE,
 session_id TEXT NOT NULL,epoch INTEGER NOT NULL,PRIMARY KEY(job_id,session_id));
CREATE INDEX IF NOT EXISTS df_learning_consolidation_source_session ON df_learning_consolidation_sources(session_id,job_id);
CREATE TABLE IF NOT EXISTS df_learning_consolidation_patterns(
 job_id TEXT NOT NULL REFERENCES df_learning_consolidations(job_id) ON DELETE CASCADE,
 pattern_id TEXT NOT NULL,PRIMARY KEY(job_id,pattern_id));
CREATE INDEX IF NOT EXISTS df_learning_consolidation_source_pattern ON df_learning_consolidation_patterns(pattern_id,job_id);
CREATE INDEX IF NOT EXISTS df_learning_pattern_revision_consolidation ON df_learning_pattern_revisions(consolidation_id,pattern_id);
CREATE INDEX IF NOT EXISTS df_learning_pattern_member_experience ON df_learning_pattern_members(experience_id,pattern_id);
`)
	return err
}

func disableWorkspaceLearningTx(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&exists); err != nil || !exists {
		return err
	}
	if err := skillmaintenance.Migrate(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE state IN ('queued','retry','running') AND id IN (
 SELECT job_id FROM df_learning_consolidation_sharing
 UNION SELECT m.job_id FROM df_learning_matches m JOIN df_learning_pattern_sharing p ON p.pattern_id=m.pattern_id)`); err != nil {
		return err
	}
	for _, query := range []string{
		`UPDATE df_learning_consolidation_inputs SET state='cancelled',reason='workspace_sharing_disabled' WHERE state='queued' AND job_id IN
 (SELECT s.job_id FROM df_learning_consolidation_sharing s JOIN df_skill_maintenance_jobs j ON j.id=s.job_id WHERE j.state='cancelled')`,
		`UPDATE df_learning_match_inputs SET state='cancelled',reason='workspace_sharing_disabled' WHERE state='queued' AND job_id IN
 (SELECT m.job_id FROM df_learning_matches m JOIN df_learning_pattern_sharing s ON s.pattern_id=m.pattern_id JOIN df_skill_maintenance_jobs j ON j.id=m.job_id WHERE j.state='cancelled')`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}
