package desktopconfig

import (
	"bytes"
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSkillLearningExperienceDiscoverySettingsSaveLaunchAndDefaultOff(t *testing.T) {
	options, doc := reviewDocument(t)
	evolution := readObject(doc, "skill_evolution")
	learning := readObject(evolution, "learning")
	discovery := readObject(evolution, "discovery")
	if discovery["experience_patterns_enabled"] != false {
		t.Fatal("experience discovery enabled by default")
	}
	evolution["generation_model_name"] = "first"
	readObject(evolution, "generation")["enabled"] = true
	learning["enabled"], learning["consolidation_enabled"], learning["matching_enabled"] = true, true, true
	discovery["experience_patterns_enabled"] = true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillLearning == nil || cfg.SkillLearning.Matching == nil || cfg.SkillGeneration == nil || cfg.SkillDiscovery == nil || !cfg.SkillDiscovery.ExperiencePatternsEnabled || !cfg.SkillDiscovery.DisableOperatorCorrections || cfg.SkillAutoPatch != nil {
		t.Fatal("experience discovery save/launch disagreement", cfg.SkillDiscovery, err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "learning")["matching_enabled"] = false
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("experience discovery without matching saved")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid edit changed effective configuration")
	}
}

func TestSkillLearningExperienceNewSkillSettingsSaveLaunchAndDefaultOff(t *testing.T) {
	options, doc := reviewDocument(t)
	e := readObject(doc, "skill_evolution")
	l, d := readObject(e, "learning"), readObject(e, "discovery")
	if d["experience_new_skills_enabled"] != false {
		t.Fatal("new capability discovery enabled by default")
	}
	e["generation_model_name"] = "first"
	readObject(e, "generation")["enabled"] = true
	l["enabled"], l["consolidation_enabled"], l["matching_enabled"] = true, true, true
	d["experience_patterns_enabled"], d["experience_new_skills_enabled"], d["new_skill_source_id"] = true, true, "project"
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
	if err != nil || cfg.SkillDiscovery == nil || !cfg.SkillDiscovery.ExperienceNewSkillsEnabled || !cfg.SkillDiscovery.ExperiencePatternsEnabled || cfg.SkillDiscovery.NewSkillSourceID != "project" || cfg.SkillAutoPatch != nil {
		t.Fatal("new capability save/launch mismatch", cfg.SkillDiscovery, err)
	}
	if field, ok := source.Settings.Fields["/skill_evolution/discovery/experience_new_skills_enabled"]; !ok || field.Value != true || field.Source != "yaml" || len(source.Settings.Fields) > 96 {
		t.Fatal("effective settings omitted creation gate or exceeded the desktop envelope", source.Settings)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "discovery")["experience_patterns_enabled"] = false
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("new capability bypassed the experience chain")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid capability opt-in changed saved configuration")
	}
}
