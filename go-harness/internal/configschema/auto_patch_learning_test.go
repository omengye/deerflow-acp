package configschema

import "testing"

func TestSkillLearningAutoPatchNativeAndDraftIndependentOptIn(t *testing.T) {
	for _, mode := range []string{"omitted", "off", "on", "review", "disabled", "no-discovery", "no-learning", "no-matching", "no-generation", "no-behavior", "no-suite", "null", "numeric"} {
		t.Run(mode, func(t *testing.T) {
			learning := map[string]any{"enabled": true, "consolidation_enabled": true, "matching_enabled": true}
			discovery := map[string]any{"experience_patterns_enabled": true}
			behavior := map[string]any{"enabled": true, "suite_path": "host-suite.json"}
			patch := map[string]any{"experience_patterns_enabled": true}
			generation := map[string]any{"enabled": true}
			raw := map[string]any{"enabled": true, "mode": "auto_patch", "learning": learning, "generation": generation, "discovery": discovery, "behavior": behavior, "auto_patch": patch,
				"budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 300, "workers": 1}}
			switch mode {
			case "omitted":
				delete(patch, "experience_patterns_enabled")
			case "off":
				patch["experience_patterns_enabled"] = false
			case "review":
				raw["mode"] = "review"
			case "disabled":
				raw["enabled"] = false
			case "no-discovery":
				discovery["experience_patterns_enabled"] = false
			case "no-learning":
				learning["enabled"] = false
			case "no-matching":
				learning["matching_enabled"] = false
			case "no-generation":
				generation["enabled"] = false
			case "no-behavior":
				behavior["enabled"] = false
			case "no-suite":
				behavior["suite_path"] = ""
			case "null":
				patch["experience_patterns_enabled"] = nil
			case "numeric":
				patch["experience_patterns_enabled"] = 1
			}
			projected, _, err := ProjectEvolution(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "omitted" || mode == "off" || mode == "on"
			if err := ValidateEvolutionDraft(projected, nil); (err == nil) != want {
				t.Fatal("draft", mode, err)
			}
			if err := ValidateNative(map[string]any{"skill_evolution": raw}); (err == nil) != want {
				t.Fatal("native", mode, err)
			}
		})
	}
	if Defaults("skill_evolution")["auto_patch"].(map[string]any)["experience_patterns_enabled"] != false {
		t.Fatal("learning automatic publication enabled by default")
	}
}
