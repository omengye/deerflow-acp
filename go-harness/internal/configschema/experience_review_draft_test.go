package configschema

import "testing"

func TestSkillLearningReviewDraftSettingsRequiresAutomaticExperienceChain(t *testing.T) {
	for _, mode := range []string{"on", "off", "no-experience", "no-generation", "no-learning", "null", "numeric"} {
		t.Run(mode, func(t *testing.T) {
			raw := map[string]any{"enabled": true, "generation": map[string]any{"enabled": true},
				"learning":  map[string]any{"enabled": true, "consolidation_enabled": true, "matching_enabled": true},
				"discovery": map[string]any{"experience_patterns_enabled": true, "experience_review_drafts_enabled": true},
				"budget":    map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
			d := raw["discovery"].(map[string]any)
			switch mode {
			case "off":
				delete(d, "experience_review_drafts_enabled")
			case "no-experience":
				d["experience_patterns_enabled"] = false
			case "no-generation":
				raw["generation"].(map[string]any)["enabled"] = false
			case "no-learning":
				raw["learning"].(map[string]any)["enabled"] = false
			case "null":
				d["experience_review_drafts_enabled"] = nil
			case "numeric":
				d["experience_review_drafts_enabled"] = 1
			}
			if err := ValidateNative(map[string]any{"skill_evolution": raw}); (err == nil) != (mode == "on" || mode == "off") {
				t.Fatal(mode, err)
			}
		})
	}
	if Defaults("skill_evolution")["discovery"].(map[string]any)["experience_review_drafts_enabled"] != false {
		t.Fatal("additional draft spending enabled by default")
	}
}
