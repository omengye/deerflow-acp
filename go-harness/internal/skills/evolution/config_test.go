package evolution

import (
	"strings"
	"testing"
)

func TestSkillMaintenanceModelInheritance(t *testing.T) {
	models := map[string]bool{"primary": true, "quality": true}
	base := "enabled: true\ngeneration:\n  enabled: true\nbudget:\n  max_daily_model_calls: 30\n  max_daily_tokens: 100000\n  max_job_seconds: 300\n  workers: 1\n"
	for _, test := range []struct {
		name, references, moderation, evaluation, generation string
	}{
		{"omitted", "", "primary", "primary", "primary"},
		{"null", "moderation_model_name: null\nevaluation_model_name: null\ngeneration_model_name: null\n", "primary", "primary", "primary"},
		{"empty", "moderation_model_name: ''\nevaluation_model_name: ''\ngeneration_model_name: ''\n", "primary", "primary", "primary"},
		{"overrides", "moderation_model_name: quality\nevaluation_model_name: null\ngeneration_model_name: quality\n", "quality", "primary", "quality"},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings, err := DecodeReviewConfig([]byte(base+test.references), models, "primary")
			if err != nil || settings.ModerationModel != test.moderation || settings.EvaluationModel != test.evaluation || settings.GenerationModel != test.generation {
				t.Fatalf("model inheritance: %+v %v", settings, err)
			}
		})
	}
	for _, test := range []struct{ references, defaultModel, errorField string }{
		{"", "", "moderation_model_name"},
		{"", "missing", "moderation_model_name"},
		{"moderation_model_name: missing\n", "primary", "moderation_model_name"},
		{"evaluation_model_name: missing\n", "primary", "evaluation_model_name"},
		{"generation_model_name: missing\n", "primary", "generation_model_name"},
		{"moderation_model_name: primary\nevaluation_model_name: quality\n", "missing", "generation_model_name"},
	} {
		if _, err := DecodeReviewConfig([]byte(base+test.references), models, test.defaultModel); err == nil || !strings.Contains(err.Error(), test.errorField) {
			t.Fatalf("invalid model reference accepted or lost field path: %+v %v", test, err)
		}
	}
	// No inherited route is needed when every active stage has an override.
	if _, err := DecodeReviewConfig([]byte(base+"moderation_model_name: primary\nevaluation_model_name: quality\ngeneration_model_name: primary\n"), models, ""); err != nil {
		t.Fatal(err)
	}
	settings, err := DecodeReviewConfig([]byte("enabled: true\nmoderation_model_name: primary\nevaluation_model_name: quality\n"), models, "")
	if err != nil || settings.GenerationModel != "" || settings.Generation.Enabled {
		t.Fatalf("inactive generation resolved a model or enabled work: %+v %v", settings, err)
	}
	if _, err := DecodeReviewConfig([]byte("enabled: false\n"), nil, ""); err != nil {
		t.Fatal("disabled maintenance required a model", err)
	}
}

func TestSkillReviewConfigSharesStrictActiveContract(t *testing.T) {
	models := map[string]bool{"primary": true, "quality": true}
	base := "enabled: true\nmoderation_model_name: primary\nevaluation_model_name: quality\n"
	settings, err := DecodeReviewConfig([]byte(base), models, "primary")
	if err != nil || !settings.Enabled || settings.Mode != "review" || settings.Validation.TimeoutSeconds != 60 || settings.Validation.MaxInputBytes != 128<<10 || settings.Validation.MaxOutputTokens != 1024 {
		t.Fatalf("defaults: %+v %v", settings, err)
	}
	for _, extra := range []string{"mode: auto_patch\n", "security_fail_closed: false\n", "unknown_setting: true\n", "discovery:\n  enabled: true\n", "validation:\n  timeout_seconds: -1\n", "validation:\n  max_input_bytes: 1\n", "validation:\n  max_output_tokens: 10000\n", "---\nenabled: false\n"} {
		if _, err := DecodeReviewConfig([]byte(base+extra), models, "primary"); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Fatalf("ignored active setting %q: %v", extra, err)
		}
	}
	if _, err := DecodeReviewConfig([]byte(strings.Replace(base, "quality", "missing", 1)), models, "primary"); err == nil || !strings.Contains(err.Error(), "evaluation_model_name") {
		t.Fatalf("unknown model: %v", err)
	}
	if _, err := DecodeReviewConfig([]byte("enabled: true\nenabled: false\n"), models, "primary"); err == nil {
		t.Fatal("ambiguous enabled setting accepted")
	}
	settings, err = DecodeReviewConfig([]byte("enabled: false\nmode: auto_patch\nlegacy_setting: retain-for-migration\nmoderation_model_name: unknown\n"), models, "primary")
	if err != nil || settings.Enabled {
		t.Fatalf("disabled legacy settings activated: %+v %v", settings, err)
	}
}

func TestGenerationConfigRequiresOptInAndExplicitBudget(t *testing.T) {
	models := map[string]bool{"primary": true, "quality": true}
	base := "enabled: true\nmoderation_model_name: primary\nevaluation_model_name: quality\ngeneration_model_name: primary\n"
	budget := "budget:\n  max_daily_model_calls: 30\n  max_daily_tokens: 100000\n  max_job_seconds: 300\n  workers: 1\n"
	for _, data := range []string{base, base + budget, strings.Replace(base, "generation_model_name: primary", "generation_model_name: historical-unknown", 1)} {
		settings, err := DecodeReviewConfig([]byte(data), models, "primary")
		if err != nil || settings.Generation.Enabled {
			t.Fatal("model reference alone enabled paid generation", err)
		}
	}
	active := base + "generation:\n  enabled: true\n"
	for _, invalid := range []string{
		active,
		active + strings.Replace(budget, "  workers: 1\n", "", 1),
		active + strings.Replace(budget, "workers: 1", "workers: 0", 1),
		active + strings.Replace(budget, "workers: 1", "workers: 1.5", 1),
		strings.Replace(active, "generation_model_name: primary", "generation_model_name: missing", 1) + budget,
		active + "generation_model_name: quality\n" + budget,
	} {
		if _, err := DecodeReviewConfig([]byte(invalid), models, "primary"); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Fatal("invalid generation configuration accepted", err)
		}
	}
	settings, err := DecodeReviewConfig([]byte(active+budget), models, "primary")
	if err != nil || !settings.Generation.Enabled || settings.Budget.Workers != 1 {
		t.Fatal("valid explicit generation rejected", err)
	}
	settings, err = DecodeReviewConfig([]byte(strings.Replace(active+budget, "enabled: true", "enabled: false", 1)), models, "primary")
	if err != nil || settings.Enabled || settings.Generation.Enabled {
		t.Fatal("disabled evolution retained generation policy", err)
	}
}
