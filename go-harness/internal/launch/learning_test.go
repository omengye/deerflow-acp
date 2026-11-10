package launch

import (
	"fmt"
	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillLearningLaunchAutomaticPrimaryInheritance(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(roleModels, "local_acp:\n", "local_acp:\n  model_name: research\n", 1)
	body += fmt.Sprintf("skills:\n  path: %s\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	source, err := ApplyConfig(writeRoleConfig(t, body), &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillLearning.Consolidation == nil || cfg.SkillLearning.Matching == nil || cfg.SkillGeneration == nil || cfg.SkillDiscovery == nil || cfg.SkillAutoPatch == nil {
		t.Fatal("automatic chain incomplete")
	}
	for _, route := range []string{cfg.SkillLearning.Model.Route.BaseURL, cfg.SkillLearning.Consolidation.Route.BaseURL, cfg.SkillLearning.Matching.Route.BaseURL, cfg.SkillGeneration.Model.Route.BaseURL} {
		if route != "https://one.invalid/v1" {
			t.Fatal("maintenance did not inherit primary")
		}
	}
	if cfg.BaseURL != "https://two.invalid/v1" || cfg.SkillLearning.Idle != 30*time.Second || !cfg.SkillLearning.CrossSession {
		t.Fatal("session selection or internal policy changed")
	}
	for key := range source.Settings.Fields {
		if strings.Contains(key, "skill_evolution") {
			t.Fatal("retired applied setting", key)
		}
	}
}

func TestSkillLearningLaunchMigratesRemovedGatesWithoutRestoringThem(t *testing.T) {
	path := writeRoleConfig(t, roleModels+"skill_evolution:\n  enabled: false\n  mode: review\n  learning:\n    enabled: false\n    matching_enabled: false\n    consolidation_model_name: absent\n  budget:\n    max_daily_model_calls: 0\n    max_daily_tokens: 0\n")
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillLearning.Matching == nil || cfg.SkillLearning.Consolidation == nil || cfg.SkillAutoPatch == nil {
		t.Fatal("old settings disabled automatic pipeline")
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "skill_evolution") {
		t.Fatal("legacy config was not removed", err)
	}
}
