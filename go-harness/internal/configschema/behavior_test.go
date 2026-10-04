package configschema

import (
	"strings"
	"testing"
)

func TestBehaviorNativeAndDraftPrerequisitesAgree(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, generation := range []bool{false, true} {
			raw := map[string]any{"enabled": enabled, "generation": map[string]any{"enabled": generation}, "behavior": map[string]any{"enabled": true, "suite_path": "fixtures.json"}}
			projected, _, err := ProjectEvolution(raw)
			if err != nil {
				t.Fatal(err)
			}
			draftErr := ValidateEvolutionDraft(projected, nil)
			nativeErr := ValidateNative(map[string]any{"skill_evolution": raw})
			if (draftErr == nil) != (enabled && generation) || (nativeErr == nil) != (enabled && generation) {
				t.Fatalf("enabled=%v generation=%v draft=%v native=%v", enabled, generation, draftErr, nativeErr)
			}
		}
	}
}

func TestBehaviorNativeLimitsAndExplicitNullCannotBeCoerced(t *testing.T) {
	for _, edit := range []struct {
		key   string
		value any
	}{
		{"timeout_seconds", 0}, {"timeout_seconds", 301}, {"max_steps", 0}, {"max_steps", 33},
		{"max_input_bytes", 1023}, {"max_input_bytes", 2097153}, {"max_output_tokens", 127}, {"max_output_tokens", 8193},
		{"suite_path", ""}, {"suite_path", nil}, {"enabled", nil}, {"enabled", "yes"},
		{"max_steps", 2.5}, {"max_steps", nil}, {"timeout_seconds", nil}, {"max_input_bytes", nil}, {"max_output_tokens", nil},
	} {
		raw := map[string]any{"enabled": true, "generation": map[string]any{"enabled": true}, "behavior": map[string]any{"enabled": true, "suite_path": "fixtures.json"}}
		raw["behavior"].(map[string]any)[edit.key] = edit.value
		if err := ValidateNative(map[string]any{"skill_evolution": raw}); err == nil || !strings.Contains(err.Error(), "behavior") {
			t.Fatalf("invalid behavior %s=%#v accepted: %v", edit.key, edit.value, err)
		}
	}
}
