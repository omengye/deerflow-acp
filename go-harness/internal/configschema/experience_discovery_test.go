package configschema

import "testing"

func TestSkillLearningExperienceDiscoveryRequiresExplicitCompleteChain(t *testing.T) {
	for _, mode := range []string{"off", "on", "no-generation", "no-learning", "no-consolidation", "no-matching", "outer-disabled", "null", "numeric"} {
		t.Run(mode, func(t *testing.T) {
			learning := map[string]any{"enabled": true, "consolidation_enabled": true, "matching_enabled": true}
			generation := map[string]any{"enabled": true}
			discovery := map[string]any{"experience_patterns_enabled": true}
			raw := map[string]any{"enabled": true, "learning": learning, "generation": generation, "discovery": discovery,
				"budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
			switch mode {
			case "off":
				delete(discovery, "experience_patterns_enabled")
			case "no-generation":
				generation["enabled"] = false
			case "no-learning":
				learning["enabled"] = false
			case "no-consolidation":
				learning["consolidation_enabled"] = false
			case "no-matching":
				learning["matching_enabled"] = false
			case "outer-disabled":
				raw["enabled"] = false
			case "null":
				discovery["experience_patterns_enabled"] = nil
			case "numeric":
				discovery["experience_patterns_enabled"] = 1
			}
			if err := ValidateNative(map[string]any{"skill_evolution": raw}); (err == nil) != (mode == "off" || mode == "on") {
				t.Fatal(mode, err)
			}
		})
	}
	if Defaults("skill_evolution")["discovery"].(map[string]any)["experience_patterns_enabled"] != false {
		t.Fatal("experience proposal spending enabled by default")
	}
}

func TestSkillLearningExperienceNewSkillRequiresSeparateOptInAndSource(t *testing.T) {
	for _, mode := range []string{"off", "on", "no-experience-discovery", "no-source", "no-generation", "no-matching", "outer-disabled", "null", "numeric"} {
		t.Run(mode, func(t *testing.T) {
			learning := map[string]any{"enabled": true, "consolidation_enabled": true, "matching_enabled": true}
			generation := map[string]any{"enabled": true}
			flags := map[string]any{"experience_patterns_enabled": true, "experience_new_skills_enabled": true, "new_skill_source_id": "project"}
			raw := map[string]any{"enabled": true, "learning": learning, "generation": generation, "discovery": flags,
				"budget": map[string]any{"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 60, "workers": 1}}
			switch mode {
			case "off":
				delete(flags, "experience_new_skills_enabled")
				delete(flags, "new_skill_source_id")
			case "no-experience-discovery":
				flags["experience_patterns_enabled"] = false
			case "no-source":
				delete(flags, "new_skill_source_id")
			case "no-generation":
				generation["enabled"] = false
			case "no-matching":
				learning["matching_enabled"] = false
			case "outer-disabled":
				raw["enabled"] = false
			case "null":
				flags["experience_new_skills_enabled"] = nil
			case "numeric":
				flags["experience_new_skills_enabled"] = 1
			}
			if err := ValidateNative(map[string]any{"skill_evolution": raw}); (err == nil) != (mode == "on" || mode == "off") {
				t.Fatal(mode, err)
			}
		})
	}
	if Defaults("skill_evolution")["discovery"].(map[string]any)["experience_new_skills_enabled"] != false {
		t.Fatal("new capability creation enabled by default")
	}
}
