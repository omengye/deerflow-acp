package configschema

import (
	"strings"
	"testing"
)

func TestAutomaticEvolutionHasNoEditableFields(t *testing.T) {
	if len(evolutionFields()) != 0 {
		t.Fatal("automatic policy exposed as customer settings")
	}
	for _, value := range []any{nil, map[string]any{}, map[string]any{"enabled": false}, map[string]any{"mode": "review"}, map[string]any{"budget": map[string]any{"max_daily_tokens": 1}}, map[string]any{"learning": map[string]any{"max_daily_model_calls": 1}}} {
		if err := validateNativeEvolution(map[string]any{"skill_evolution": value}); err == nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("obsolete settings accepted: %v", err)
		}
	}
	if err := validateNativeEvolution(map[string]any{}); err != nil {
		t.Fatal(err)
	}
}
func TestOldEvolutionDraftIsRejected(t *testing.T) {
	for _, key := range []string{"enabled", "mode", "generation", "learning", "discovery", "behavior", "auto_patch", "budget", "moderation_model_name"} {
		if err := ValidateEvolutionDraft(map[string]any{key: false}, nil); err == nil {
			t.Fatal(key)
		}
	}
}
