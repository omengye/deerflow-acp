package configschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Automatic skill evolution has no customer-editable settings.
func evolutionFields() []Field { return nil }

func removedEvolutionSettings(document map[string]any) error {
	if len(document) != 0 {
		return fmt.Errorf("skill_evolution settings have been removed; use the automatic configuration migration")
	}
	return nil
}

func ProjectEvolution(raw map[string]any) (map[string]any, []string, error) {
	return map[string]any{}, nil, removedEvolutionSettings(raw)
}
func ValidateEvolutionDraft(document, dormant map[string]any) error {
	return removedEvolutionSettings(document)
}
func ValidateEvolutionWorkerPrerequisites(document map[string]any) error {
	return removedEvolutionSettings(document)
}
func ValidateEvolutionLearningPrerequisites(document map[string]any) error {
	return removedEvolutionSettings(document)
}
func ValidateEvolutionDiscoveryPrerequisites(document map[string]any) error {
	return removedEvolutionSettings(document)
}
func validateNativeEvolution(document map[string]any) error {
	if _, present := document["skill_evolution"]; present {
		return fmt.Errorf("skill_evolution configuration has been removed")
	}
	return nil
}
func isEvolutionField(field Field) bool { return strings.HasPrefix(field.Path, "/skill_evolution") }

// JSON/YAML integer representations differ after the desktop round trip.
func SameValue(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
