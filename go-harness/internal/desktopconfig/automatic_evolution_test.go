package desktopconfig

import (
	"bytes"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestAutomaticEvolutionZeroConfigurationAndPrimaryInheritance(t *testing.T) {
	options, doc := reviewDocument(t)
	for _, primary := range []string{"first", "second"} {
		doc["default_model"] = primary
		readObject(doc, "runtime")["model_name"] = "second"
		doc = run(t, options, "save", doc)
		if _, exists := doc["skill_evolution"]; exists {
			t.Fatal("snapshot reintroduced retired settings")
		}
		raw, err := os.ReadFile(options.Config)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("skill_evolution")) {
			t.Fatal("save reintroduced retired configuration")
		}
		var cfg deerflow.Config
		connections := 0
		if _, err = launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
			t.Fatal(err)
		}
		if cfg.SkillGeneration == nil || cfg.SkillBehaviorEvaluator != nil || !cfg.SkillGeneration.PublishAfterSecurityReview || cfg.SkillLearning == nil || cfg.SkillDiscovery == nil {
			t.Fatal("automatic pipeline is incomplete")
		}
		want := "gpt-a"
		if primary == "second" {
			want = "gpt-b"
		}
		if cfg.SkillGeneration.Model.Route.Model != want {
			t.Fatal("maintenance failed to inherit the primary model")
		}
	}
}

func TestAutomaticEvolutionRejectsLegacyClientFieldsWithoutWriting(t *testing.T) {
	options, doc := reviewDocument(t)
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"skill_evolution", "skill_evolution_migration_ack", "skill_evolution_migration"} {
		doc[key] = map[string]any{"enabled": false, "mode": "manual", "max_daily_tokens": 1, "max_daily_model_calls": 1}
		for _, op := range []string{"validate", "save"} {
			if _, err = Run(options, op, mustJSON(t, doc)); err == nil {
				t.Fatalf("accepted retired client field %s", key)
			}
		}
		delete(doc, key)
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid legacy client request changed config")
	}
}

func TestAutomaticEvolutionMigratesOnceAndPreservesOtherSettings(t *testing.T) {
	options := setup(t)
	var legacy map[string]any
	if err := yaml.Unmarshal([]byte(fixture), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["skill_evolution"] = map[string]any{"enabled": false, "mode": "review", "budget": map[string]any{"max_daily_tokens": 1, "max_daily_model_calls": 1}}
	body, _ := yaml.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(options.Resources, "default-config.yaml"), body, 0600); err != nil {
		t.Fatal(err)
	}
	run(t, options, "init", nil)
	doc := run(t, options, "snapshot", nil)
	run(t, options, "save", doc)
	raw, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("skill_evolution")) || !bytes.Contains(raw, []byte("first-private-key")) || !bytes.Contains(raw, []byte("session_cleanup_enabled: false")) {
		t.Fatal("migration lost unrelated config or left retired fields")
	}
	if err = os.WriteFile(options.Config, append(raw, []byte("\nskill_evolution: {}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(options, "snapshot", nil); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatal("migration accepted reintroduced settings", err)
	}
}
