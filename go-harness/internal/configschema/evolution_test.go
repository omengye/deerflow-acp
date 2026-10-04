package configschema

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestEvolutionNativeFractionsCannotBeCoercedToIntegers(t *testing.T) {
	for _, key := range []string{"timeout_seconds", "max_input_bytes", "max_output_tokens"} {
		var document map[string]any
		data := "config_version: eino-harness-v1\nskill_evolution:\n  enabled: true\n  validation:\n    " + key + ": 128.5\n"
		if err := yaml.Unmarshal([]byte(data), &document); err != nil {
			t.Fatal(err)
		}
		if err := ValidateNative(document); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("fraction bypassed native review schema: %v", err)
		}
	}
}

func TestEvolutionNullAndZeroDefaultsAreValidAndDormantExceptionsAreNarrow(t *testing.T) {
	projection, ignored, err := ProjectEvolution(map[string]any{"enabled": nil, "mode": "", "security_fail_closed": nil,
		"validation": map[string]any{"timeout_seconds": nil, "max_input_bytes": nil, "max_output_tokens": 0}})
	if err != nil || len(ignored) != 0 {
		t.Fatal(err, ignored)
	}
	if err := ValidateEvolutionDraft(projection, nil); err != nil {
		t.Fatal(err)
	}
	if projection["enabled"] != false || projection["mode"] != "review" || projection["security_fail_closed"] != true {
		t.Fatal("null/default policy changed")
	}
	limits := projection["validation"].(map[string]any)
	for key, value := range limits {
		if value != 0 {
			t.Fatalf("%s default changed: %v", key, value)
		}
	}
	projection["mode"] = "auto_patch"
	previous, _, _ := ProjectEvolution(projection)
	if err := ValidateEvolutionDraft(projection, previous); err != nil {
		t.Fatal("unchanged dormant value rejected", err)
	}
	projection["mode"] = "unknown"
	if err := ValidateEvolutionDraft(projection, previous); err == nil {
		t.Fatal("new dormant unsupported edit accepted")
	}
}

func TestDiscoveryNativeAndDraftPrerequisitesAgree(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, generation := range []bool{false, true} {
			raw := map[string]any{"enabled": enabled, "generation": map[string]any{"enabled": generation}, "discovery": map[string]any{"operator_corrections_enabled": true}}
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

func TestDiscoveryNativeFractionAndNullAreRejected(t *testing.T) {
	for _, key := range []string{"repeat_threshold", "repeat_window_days", "cooldown_hours", "max_daily_proposals", "max_pending_proposals"} {
		for _, value := range []any{2.5, nil} {
			raw := map[string]any{"enabled": true, "discovery": map[string]any{key: value}}
			if err := ValidateNative(map[string]any{"skill_evolution": raw}); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("%s accepted %#v: %v", key, value, err)
			}
		}
	}
}
