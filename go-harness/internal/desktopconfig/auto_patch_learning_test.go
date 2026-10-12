package desktopconfig

import (
	"bytes"
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSkillLearningAutoPatchSettingsSaveLaunchAndDefaultOff(t *testing.T) {
	options, doc := behaviorDocument(t)
	e := readObject(doc, "skill_evolution")
	patch := readObject(e, "auto_patch")
	if patch["experience_patterns_enabled"] != false {
		t.Fatal("learning publication enabled by default")
	}
	e["mode"] = "auto_patch"
	l := readObject(e, "learning")
	l["enabled"], l["consolidation_enabled"], l["matching_enabled"] = true, true, true
	l["max_daily_model_calls"] = 3
	readObject(e, "discovery")["experience_patterns_enabled"] = true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillAutoPatch == nil || cfg.SkillAutoPatch.ExperiencePatternsEnabled {
		t.Fatal("old auto_patch implicitly enabled learning publication", cfg.SkillAutoPatch, err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "auto_patch")["experience_patterns_enabled"] = true
	run(t, options, "save", doc)
	source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
	if err != nil || cfg.SkillAutoPatch == nil || !cfg.SkillAutoPatch.ExperiencePatternsEnabled || cfg.SkillLearning == nil || cfg.SkillDiscovery == nil || cfg.SkillBehavior == nil {
		t.Fatal("learning publication save/launch mismatch", cfg.SkillAutoPatch, err)
	}
	if field, ok := source.Settings.Fields["/skill_evolution/auto_patch/experience_patterns_enabled"]; !ok || field.Value != true || field.Source != "yaml" || len(source.Settings.Fields) > 96 {
		t.Fatal("learning publication missing from effective settings or exceeded desktop bounds", source.Settings)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(doc, "skill_evolution")["mode"] = "review"
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("learning automatic publication granted in review mode")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid grant changed effective configuration")
	}
}
