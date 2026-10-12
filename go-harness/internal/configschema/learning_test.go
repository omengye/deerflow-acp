package configschema

import (
	"strings"
	"testing"
)

func TestSkillLearningSchemaIndependentOptInAndSublimits(t *testing.T) {
	for _, mode := range []string{"off", "shadow", "no-budget", "zero-budget", "over-calls", "over-tokens", "fraction", "null-idle", "external", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			raw := map[string]any{"enabled": true, "generation": map[string]any{"enabled": false}, "learning": map[string]any{"enabled": true}, "budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
			learning := raw["learning"].(map[string]any)
			budget := raw["budget"].(map[string]any)
			switch mode {
			case "off":
				raw = map[string]any{}
			case "no-budget":
				delete(raw, "budget")
			case "zero-budget":
				budget["max_daily_model_calls"] = 0
			case "over-calls":
				learning["max_daily_model_calls"] = 31
			case "over-tokens":
				learning["max_daily_tokens"] = 100001
			case "fraction":
				learning["idle_seconds"] = 1.5
			case "null-idle":
				learning["idle_seconds"] = nil
			case "external":
				learning["external_context_policy"] = "raw"
			case "unknown":
				learning["experience_discovery_enabled"] = true
			}
			err := ValidateNative(map[string]any{"skill_evolution": raw})
			want := mode == "off" || mode == "shadow"
			if (err == nil) != want {
				t.Fatalf("native=%v want=%v", err, want)
			}
			if err != nil && !strings.Contains(err.Error(), "skill_evolution") {
				t.Fatal(err)
			}
		})
	}
}

func TestSkillLearningConsolidationSchemaRequiresSeparateOptIn(t *testing.T) {
	for _, mode := range []string{"off", "on", "without-learning", "null-toggle", "fraction-toggle", "null-model"} {
		raw := map[string]any{"enabled": true, "learning": map[string]any{"enabled": true, "consolidation_enabled": false}, "budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
		learning := raw["learning"].(map[string]any)
		switch mode {
		case "on":
			learning["consolidation_enabled"] = true
		case "without-learning":
			learning["enabled"], learning["consolidation_enabled"] = false, true
		case "null-toggle":
			learning["consolidation_enabled"] = nil
		case "fraction-toggle":
			learning["consolidation_enabled"] = 1.5
		case "null-model":
			learning["consolidation_enabled"], learning["consolidation_model_name"] = true, nil
		}
		err := ValidateNative(map[string]any{"skill_evolution": raw})
		if (err == nil) != (mode == "off" || mode == "on" || mode == "null-model") {
			t.Fatalf("%s=%v", mode, err)
		}
	}
}

func TestSkillLearningMatchingSchemaRequiresBothEarlierStages(t *testing.T) {
	for _, mode := range []string{"off", "on", "no-extraction", "no-consolidation", "null", "fraction", "nullable-model"} {
		learning := map[string]any{"enabled": true, "consolidation_enabled": true, "matching_enabled": true}
		raw := map[string]any{"enabled": true, "learning": learning, "budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
		switch mode {
		case "off":
			learning["matching_enabled"] = false
		case "no-extraction":
			learning["enabled"] = false
		case "no-consolidation":
			learning["consolidation_enabled"] = false
		case "null":
			learning["matching_enabled"] = nil
		case "fraction":
			learning["matching_enabled"] = 1.5
		case "nullable-model":
			learning["matching_model_name"] = nil
		}
		err := ValidateNative(map[string]any{"skill_evolution": raw})
		if (err == nil) != (mode == "off" || mode == "on" || mode == "nullable-model") {
			t.Fatal(mode, err)
		}
	}
}

func TestSkillLearningWorkspaceSchemaRequiresConsolidationAndExplicitOptIn(t *testing.T) {
	for _, mode := range []string{"off", "on", "no-learning", "no-consolidation", "null", "numeric"} {
		learning := map[string]any{"enabled": true, "consolidation_enabled": true, "cross_session_enabled": true}
		raw := map[string]any{"enabled": true, "learning": learning, "budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
		switch mode {
		case "off":
			delete(learning, "cross_session_enabled")
		case "no-learning":
			learning["enabled"] = false
		case "no-consolidation":
			learning["consolidation_enabled"] = false
		case "null":
			learning["cross_session_enabled"] = nil
		case "numeric":
			learning["cross_session_enabled"] = 1
		}
		if err := ValidateNative(map[string]any{"skill_evolution": raw}); (err == nil) != (mode == "off" || mode == "on") {
			t.Fatal(mode, err)
		}
	}
	if Defaults("skill_evolution")["learning"].(map[string]any)["cross_session_enabled"] != false {
		t.Fatal("workspace sharing enabled by default")
	}
}
