package desktopconfig

import (
	"bytes"
	"os"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSkillLearningSettingsRoundTripAndSparseRoute(t *testing.T) {
	options, doc := reviewDocument(t)
	evolution := readObject(doc, "skill_evolution")
	learning := readObject(evolution, "learning")
	if learning["enabled"] != false || learning["extraction_model_name"] != nil {
		t.Fatal("learning enabled or expanded by default")
	}
	learning["enabled"] = true
	learning["idle_seconds"] = 7
	// The fixture enables review but leaves generation off; extraction must
	// remain usable without creating publication authority.
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillGeneration != nil || cfg.SkillDiscovery != nil {
		t.Fatal("learning activated unrelated stages")
	}
	saved, err := readYAML(options.Config)
	if err != nil || readObject(readObject(saved, "skill_evolution"), "learning")["extraction_model_name"] != nil {
		t.Fatal("model inheritance was expanded", err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "learning")["extraction_model_name"] = "missing-model"
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("unknown extraction model accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid learning edit modified configuration")
	}
}

func TestSkillLearningConsolidationSettingsRoundTripAndModelValidation(t *testing.T) {
	options, doc := reviewDocument(t)
	learning := readObject(readObject(doc, "skill_evolution"), "learning")
	if learning["consolidation_enabled"] != false || learning["consolidation_model_name"] != nil {
		t.Fatal("consolidation default grants paid work")
	}
	learning["enabled"], learning["consolidation_enabled"] = true, true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillLearning == nil || cfg.SkillLearning.Consolidation == nil || cfg.SkillGeneration != nil {
		t.Fatal("settings/launch disagree", err)
	}
	saved, err := readYAML(options.Config)
	if err != nil || readObject(readObject(saved, "skill_evolution"), "learning")["consolidation_model_name"] != nil {
		t.Fatal("consolidation inheritance expanded", err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "learning")["consolidation_model_name"] = "missing-model"
	before, _ := os.ReadFile(options.Config)
	if _, err = Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("unknown consolidation model accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid consolidation edit modified disk")
	}
}

func TestSkillLearningMatchingSettingsRoundTripAndSparseModel(t *testing.T) {
	options, doc := reviewDocument(t)
	learning := readObject(readObject(doc, "skill_evolution"), "learning")
	if learning["matching_enabled"] != false || learning["matching_model_name"] != nil {
		t.Fatal("matching default grants model work")
	}
	learning["enabled"], learning["consolidation_enabled"], learning["matching_enabled"] = true, true, true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillLearning == nil || cfg.SkillLearning.Matching == nil || cfg.SkillGeneration != nil {
		t.Fatal("save/launch matching contract mismatch", err)
	}
	saved, err := readYAML(options.Config)
	if err != nil || readObject(readObject(saved, "skill_evolution"), "learning")["matching_model_name"] != nil {
		t.Fatal("model inheritance expanded", err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "learning")["matching_model_name"] = "missing-model"
	before, _ := os.ReadFile(options.Config)
	if _, err = Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("unknown matching model accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid matching edit changed disk")
	}
}

func TestSkillLearningWorkspaceSettingsRoundTripAndSeparateConsentGate(t *testing.T) {
	options, doc := reviewDocument(t)
	learning := readObject(readObject(doc, "skill_evolution"), "learning")
	if learning["cross_session_enabled"] != false {
		t.Fatal("workspace learning grants authorization by default")
	}
	learning["enabled"], learning["consolidation_enabled"], learning["cross_session_enabled"] = true, true, true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillLearning == nil || !cfg.SkillLearning.CrossSession || cfg.SkillGeneration != nil {
		t.Fatal("workspace settings/launch mismatch", err)
	}
	doc = run(t, options, "snapshot", nil)
	readObject(readObject(doc, "skill_evolution"), "learning")["consolidation_enabled"] = false
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("cross-session enabled without consolidation")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid workspace gate modified effective configuration")
	}
}
