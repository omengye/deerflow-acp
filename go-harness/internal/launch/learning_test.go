package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestSkillLearningLaunchIndependentRouteAndExplicitBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	base := strings.Replace(roleModels, "local_acp:\n", "local_acp:\n  model_name: research\n", 1)
	base += fmt.Sprintf("skills:\n  path: %s\nskill_evolution:\n  enabled: true\n  learning:\n    enabled: true\n    extraction_model_name: main\n    idle_seconds: 2\n    max_daily_model_calls: 1\n    max_daily_tokens: 30000\n  budget:\n    max_daily_model_calls: 3\n    max_daily_tokens: 100000\n    max_job_seconds: 30\n    workers: 1\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(writeRoleConfig(t, base), &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillLearning.Model.Route.BaseURL != "https://one.invalid/v1" || cfg.SkillLearning.Model.Route.APIKey != "fixture-one" || cfg.BaseURL != "https://two.invalid/v1" || cfg.SkillLearning.Idle != 2*time.Second || cfg.SkillLearning.MaxDailyModelCalls != 1 || cfg.SkillGeneration != nil || cfg.SkillDiscovery != nil {
		t.Fatal("learning route/budget/independent gates incorrect")
	}
	for _, invalid := range []string{strings.Replace(base, "max_daily_model_calls: 3", "max_daily_model_calls: 0", 1), strings.Replace(base, "extraction_model_name: main", "extraction_model_name: absent", 1)} {
		if _, err := ApplyConfig(writeRoleConfig(t, invalid), &cfg, &connections, nil); err == nil {
			t.Fatal("invalid learning configuration accepted")
		}
	}
}

func TestSkillLearningConsolidationLaunchRouteAndInactiveReference(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	base := roleModels + fmt.Sprintf("skills:\n  path: %s\nskill_evolution:\n  enabled: true\n  learning:\n    enabled: true\n    extraction_model_name: main\n    consolidation_enabled: true\n    consolidation_model_name: research\n  budget:\n    max_daily_model_calls: 30\n    max_daily_tokens: 100000\n    max_job_seconds: 60\n    workers: 1\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(writeRoleConfig(t, base), &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillLearning.Consolidation == nil || cfg.SkillLearning.Consolidation.Route.BaseURL != "https://two.invalid/v1" || cfg.SkillLearning.Model.Route.BaseURL != "https://one.invalid/v1" || cfg.SkillGeneration != nil || cfg.SkillDiscovery != nil {
		t.Fatal("consolidation route or independent gates incorrect")
	}
	invalid := strings.Replace(base, "consolidation_model_name: research", "consolidation_model_name: absent", 1)
	if _, err := ApplyConfig(writeRoleConfig(t, invalid), &cfg, &connections, nil); err == nil {
		t.Fatal("unknown active consolidation route accepted")
	}
	dormant := strings.Replace(invalid, "consolidation_enabled: true", "consolidation_enabled: false", 1)
	if _, err := ApplyConfig(writeRoleConfig(t, dormant), &cfg, &connections, nil); err != nil || cfg.SkillLearning.Consolidation != nil {
		t.Fatal("inactive consolidation became spending authority", err)
	}
}

func TestSkillLearningMatchingLaunchRouteAndIndependentGate(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	base := roleModels + fmt.Sprintf("skills:\n  path: %s\nskill_evolution:\n  enabled: true\n  learning:\n    enabled: true\n    consolidation_enabled: true\n    matching_enabled: true\n    matching_model_name: research\n  budget:\n    max_daily_model_calls: 30\n    max_daily_tokens: 100000\n    max_job_seconds: 60\n    workers: 1\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(writeRoleConfig(t, base), &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillLearning == nil || cfg.SkillLearning.Matching == nil || cfg.SkillLearning.Matching.Route.BaseURL != "https://two.invalid/v1" || cfg.SkillLearning.Model.Route.BaseURL != "https://one.invalid/v1" || cfg.SkillGeneration != nil || cfg.SkillDiscovery != nil {
		t.Fatal("matching route or paid-work isolation incorrect")
	}
	invalid := strings.Replace(base, "matching_model_name: research", "matching_model_name: absent", 1)
	if _, err := ApplyConfig(writeRoleConfig(t, invalid), &cfg, &connections, nil); err == nil {
		t.Fatal("unknown active matching route accepted")
	}
	dormant := strings.Replace(invalid, "matching_enabled: true", "matching_enabled: false", 1)
	if _, err := ApplyConfig(writeRoleConfig(t, dormant), &cfg, &connections, nil); err != nil || cfg.SkillLearning.Matching != nil {
		t.Fatal("inactive matching became spending authority", err)
	}
}

func TestSkillLearningWorkspaceLaunchGateAndAppliedSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	base := roleModels + fmt.Sprintf("skills:\n  path: %s\nskill_evolution:\n  enabled: true\n  learning:\n    enabled: true\n    consolidation_enabled: true\n    cross_session_enabled: true\n  budget:\n    max_daily_model_calls: 30\n    max_daily_tokens: 100000\n    max_job_seconds: 60\n    workers: 1\n", filepath.ToSlash(root))
	var cfg deerflow.Config
	connections := 0
	source, err := ApplyConfig(writeRoleConfig(t, base), &cfg, &connections, nil)
	if err != nil || cfg.SkillLearning == nil || !cfg.SkillLearning.CrossSession || cfg.SkillLearning.Consolidation == nil || cfg.SkillGeneration != nil {
		t.Fatal("workspace sharing launch contract mismatch", err)
	}
	if field, exists := source.Settings.Fields["/skill_evolution/learning/cross_session_enabled"]; !exists || field.Value != true {
		t.Fatal("applied settings omit effective workspace gate", source.Settings)
	}
	if _, err := ApplyConfig(writeRoleConfig(t, strings.Replace(base, "cross_session_enabled: true", "cross_session_enabled: false", 1)), &cfg, &connections, nil); err != nil || cfg.SkillLearning.CrossSession {
		t.Fatal("disabled workspace gate widened authorization", err)
	}
	if _, err := ApplyConfig(writeRoleConfig(t, strings.Replace(base, "consolidation_enabled: true", "consolidation_enabled: false", 1)), &cfg, &connections, nil); err == nil {
		t.Fatal("workspace gate accepted without consolidation")
	}
}
