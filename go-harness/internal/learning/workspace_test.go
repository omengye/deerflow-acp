package learning

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillLearningWorkspaceOptionsCannotGrantSharingWithoutConsolidation(t *testing.T) {
	o := harness.SkillLearningOptions{CrossSession: true, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 10, MaxDailyTokens: 10000, MaxJobSeconds: 60, Workers: 1}, MaxDailyModelCalls: 5, MaxDailyTokens: 5000}
	if validateOptions(o) == nil {
		t.Fatal("cross-session accepted without consolidation")
	}
	o.Consolidation = &harness.SkillGenerationModelConfig{}
	if err := validateOptions(o); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningWorkspaceLineageMigrationIsOneTimeAndDoesNotChangeLegacyKey(t *testing.T) {
	db, err := sqlite.Open(t.TempDir() + "/lineage.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err = db.DB().Exec(`CREATE TABLE harness_sessions(id TEXT PRIMARY KEY);
 CREATE TABLE df_learning_episodes(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,independence_key TEXT NOT NULL);
 CREATE TABLE df_learning_migrations(name TEXT PRIMARY KEY);
 CREATE TABLE df_learning_patterns(id TEXT PRIMARY KEY);
 CREATE TABLE df_learning_pattern_revisions(consolidation_id TEXT,pattern_id TEXT);
 CREATE TABLE df_learning_pattern_members(experience_id TEXT,pattern_id TEXT);
 CREATE TABLE df_learning_consolidations(job_id TEXT PRIMARY KEY);
 INSERT INTO harness_sessions VALUES('s'); INSERT INTO df_learning_episodes VALUES('e','s','original-key');`); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		tx, err := db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = migrateWorkspaceLearning(ctx, tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = db.DB().QueryRow(`SELECT count(*) FROM df_learning_episode_lineage`).Scan(&count); err != nil || count != 1-i {
			t.Fatal("migration recreated removed lineage", count, err)
		}
		if i == 0 {
			var key string
			if err = db.DB().QueryRow(`SELECT independence_key FROM df_learning_episode_lineage`).Scan(&key); err != nil || key != "original-key" {
				t.Fatal("migration changed pinned legacy key", key, err)
			}
			if _, err = db.DB().Exec(`DELETE FROM df_learning_episode_lineage`); err != nil {
				t.Fatal(err)
			}
		}
	}
}
