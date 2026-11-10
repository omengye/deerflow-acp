package configschema

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestEvolutionMigrationRemovesOnlyRetiredConfiguration(t *testing.T) {
	raw := []byte("# primary model\nmodels:\n  - name: main\n    api_key: ${MODEL_KEY}\nskills:\n  managed_sources: []\nskill_evolution:\n  enabled: false\n  mode: review\n  budget:\n    max_daily_model_calls: 1\n    max_daily_tokens: 1\nother:\n  enabled: false\n")
	result, changed, err := MigrateEvolutionYAML(raw)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	var before, after map[string]any
	if yaml.Unmarshal(raw, &before) != nil || yaml.Unmarshal(result, &after) != nil {
		t.Fatal("bad YAML")
	}
	delete(before, "skill_evolution")
	if !SameValue(before, after) || !bytes.Contains(result, []byte("# primary model")) || !bytes.Contains(result, []byte("${MODEL_KEY}")) {
		t.Fatal("unrelated configuration changed")
	}
	again, changed, err := MigrateEvolutionYAML(result)
	if err != nil || changed || !bytes.Equal(again, result) {
		t.Fatal("migration not idempotent", err)
	}
}

func TestEvolutionMigrationFileResumesPreparedJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("default_model: main\nskill_evolution: {enabled: true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateEvolutionFile(path); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(filepath.Dir(path), ".skill-evolution-migration.json")
	body, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.ReplaceAll(body, []byte("config_written"), []byte("prepared"))
	if err = os.WriteFile(journalPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = MigrateEvolutionFile(path); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(journalPath)
	if err != nil || !bytes.Contains(body, []byte("config_written")) {
		t.Fatal("journal not recovered", err)
	}
}

func TestEvolutionMigrationRejectsAmbiguousYAML(t *testing.T) {
	for _, raw := range []string{"skill_evolution: {}\nskill_evolution: {}\n", "skill_evolution: {}\n---\nother: true\n"} {
		if _, _, err := MigrateEvolutionYAML([]byte(raw)); err == nil {
			t.Fatal("ambiguous config accepted")
		}
	}
}

func TestEvolutionMigrationDatabaseCommitAndFinalFence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("default_model: main\nskill_evolution: {enabled: false}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateEvolutionFile(path); err != nil {
		t.Fatal(err)
	}
	id, err := EvolutionMigrationID(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE harness_events(sequence INTEGER);CREATE TABLE harness_runs(id TEXT,session_id TEXT,input_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err = skillmaintenance.BeginAutomaticEpoch(skillmaintenance.WithMigrationID(ctx, id), tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Crash after DB commit, before the file marker: the same journal resumes.
	if err = MigrateEvolutionFile(path); err != nil {
		t.Fatal(err)
	}
	if err = CompleteEvolutionFile(ctx, path, db.DB()); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(filepath.Dir(path), ".skill-evolution-migration.json")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var j evolutionConfigJournal
	if json.Unmarshal(before, &j) != nil || j.State != "complete" || j.Epoch == "" || j.ID != id {
		t.Fatal("incomplete migration binding")
	}
	if err = CompleteEvolutionFile(ctx, path, db.DB()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(journal)
	if !bytes.Equal(before, after) {
		t.Fatal("restart moved migration marker")
	}
	if _, err = db.DB().Exec(`UPDATE df_evolution_epoch SET input_watermark=input_watermark+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = CompleteEvolutionFile(ctx, path, db.DB()); err == nil {
		t.Fatal("changed database watermark accepted")
	}
}
