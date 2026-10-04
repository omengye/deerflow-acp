package desktopconfig

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestDiscoverySettingsSaveValidateLaunchRoundTrip(t *testing.T) {
	options, doc := generationDocument(t)
	discovery := readObject(readObject(doc, "skill_evolution"), "discovery")
	if discovery["operator_corrections_enabled"] != false {
		t.Fatal("snapshot enabled automatic discovery by default")
	}
	discovery["operator_corrections_enabled"] = true
	discovery["repeat_threshold"], discovery["repeat_window_days"], discovery["cooldown_hours"] = 3, 4, 5
	discovery["max_daily_proposals"], discovery["max_pending_proposals"] = 6, 7
	before, _ := os.ReadFile(options.Config)
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("valid discovery settings rejected")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("validation changed config")
	}
	doc = run(t, options, "save", doc)
	run(t, options, "validate", doc)
	if !reflect.DeepEqual(readObject(readObject(doc, "skill_evolution"), "discovery"), discovery) {
		t.Fatal("save response lost discovery settings")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	want := &harness.SkillDiscoveryOptions{RepeatThreshold: 3, RepeatWindowDays: 4, CooldownHours: 5, MaxDailyProposals: 6, MaxPendingProposals: 7}
	if !reflect.DeepEqual(cfg.SkillDiscovery, want) || cfg.SkillGeneration == nil || cfg.SkillGeneration.Budget.MaxDailyModelCalls != 3 {
		t.Fatalf("launch lost discovery or generation budget: %+v %+v", cfg.SkillDiscovery, cfg.SkillGeneration)
	}
	readObject(readObject(doc, "skill_evolution"), "discovery")["operator_corrections_enabled"] = false
	doc = run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillDiscovery != nil || cfg.SkillGeneration == nil {
		t.Fatalf("discovery disable did not clear selector alone: %+v %v", cfg.SkillDiscovery, err)
	}
	readObject(doc, "skill_evolution")["enabled"] = false
	run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillDiscovery != nil || cfg.SkillGeneration != nil {
		t.Fatalf("evolution disable retained paid workers: %+v %v", cfg.SkillDiscovery, err)
	}
}

func TestDiscoverySettingsSaveAndLaunchRejectSameInvalidEdits(t *testing.T) {
	for _, edit := range []struct {
		group, key string
		value      any
	}{
		{"discovery", "repeat_threshold", 1}, {"discovery", "repeat_threshold", 17}, {"discovery", "repeat_threshold", 2.5},
		{"discovery", "repeat_window_days", 366}, {"discovery", "cooldown_hours", 8761},
		{"discovery", "max_daily_proposals", 101}, {"discovery", "max_pending_proposals", 1001},
		{"discovery", "operator_corrections_enabled", "yes"}, {"discovery", "repeat_threshold", nil},
		{"", "enabled", false}, {"generation", "enabled", false},
		{"", "generation_model_name", "missing"}, {"budget", "max_daily_tokens", 0},
	} {
		t.Run(fmt.Sprintf("%s-%s-%v", edit.group, edit.key, edit.value), func(t *testing.T) {
			options, doc := generationDocument(t)
			review := readObject(doc, "skill_evolution")
			readObject(review, "discovery")["operator_corrections_enabled"] = true
			target := review
			if edit.group != "" {
				target = readObject(review, edit.group)
			}
			target[edit.key] = edit.value
			before, _ := os.ReadFile(options.Config)
			for _, op := range []string{"validate", "save"} {
				if _, err := Run(options, op, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
					t.Fatalf("%s accepted invalid edit: %v", op, err)
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected edit changed file")
			}
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			raw["skill_evolution"] = review
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
				t.Fatalf("launch accepted invalid edit: %v", err)
			}
		})
	}
}

func TestDiscoveryLegacyFlagsNeverEnableNativeSelector(t *testing.T) {
	options, _ := generationDocument(t)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	raw["skill_evolution"] = map[string]any{"enabled": false, "discovery": map[string]any{"enabled": true, "min_tool_calls": 5}}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	want := []string{"skill_evolution.discovery.enabled", "skill_evolution.discovery.min_tool_calls"}
	if !reflect.DeepEqual(readObject(doc, "skill_evolution_migration")["removed_paths"], want) || readObject(readObject(doc, "skill_evolution"), "discovery")["operator_corrections_enabled"] != false {
		t.Fatal("legacy flags lost migration diagnostics or enabled selector")
	}
	doc = run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil || readObject(readObject(saved, "skill_evolution"), "discovery")["enabled"] != true {
		t.Fatal("unrelated save dropped legacy flag", err)
	}
	doc["skill_evolution_migration_ack"] = doc["config_revision"]
	doc = run(t, options, "save", doc)
	if readObject(readObject(doc, "skill_evolution"), "discovery")["operator_corrections_enabled"] != false || readObject(doc, "skill_evolution_migration")["required"] != false {
		t.Fatal("conversion enabled discovery or retained legacy flags")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillDiscovery != nil {
		t.Fatal("converted dormant settings activated selector", err)
	}
}
