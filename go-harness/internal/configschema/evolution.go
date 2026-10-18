package configschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Expose only consumed settings. Operator corrections have a separate native
// opt-in; legacy discovery.enabled must not grant new model-spending authority.
func evolutionFields() []Field {
	return []Field{
		{Path: "/skill_evolution_migration_ack", Type: "string", Default: ""},
		{Path: "/skill_evolution/enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/mode", Type: "string", Default: "review", Enum: []string{"review", "auto_patch"}},
		{Path: "/skill_evolution/allow_builtin_overlays", Type: "boolean", Default: false},
		{Path: "/skill_evolution/moderation_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/evaluation_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/generation_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/security_fail_closed", Type: "boolean", Default: true, MustBeTrue: true},
		{Path: "/skill_evolution/validation", Type: "review_limits", Default: map[string]any{
			"timeout_seconds": 0, "max_input_bytes": 0, "max_output_tokens": 0, "response_format": "text",
		}},
		reviewLimit("timeout_seconds", 1, 300),
		reviewLimit("max_input_bytes", 1024, 2<<20),
		reviewLimit("max_output_tokens", 128, 8192),
		{Path: "/skill_evolution/validation/response_format", Type: "string", Default: "text", Enum: []string{"text", "json_schema"}},
		{Path: "/skill_evolution/generation", Type: "generation_settings", Default: map[string]any{
			"enabled": false, "timeout_seconds": 0, "max_input_bytes": 0, "max_output_tokens": 0,
		}},
		{Path: "/skill_evolution/generation/enabled", Type: "boolean", Default: false},
		generationLimit("timeout_seconds", 1, 300),
		generationLimit("max_input_bytes", 1024, 2<<20),
		generationLimit("max_output_tokens", 128, 32768),
		{Path: "/skill_evolution/learning", Type: "learning_settings", Default: map[string]any{
			"enabled": false, "extraction_model_name": nil, "consolidation_enabled": false, "consolidation_model_name": nil, "cross_session_enabled": false, "matching_enabled": false, "matching_model_name": nil, "idle_seconds": 300,
			"timeout_seconds": 60, "max_input_bytes": 131072, "max_output_tokens": 4096,
			"max_daily_model_calls": 10, "max_daily_tokens": 50000, "external_context_policy": "hints_only",
		}},
		{Path: "/skill_evolution/learning/enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/learning/extraction_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/learning/consolidation_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/learning/consolidation_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/learning/cross_session_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/learning/matching_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/learning/matching_model_name", Type: "string", Nullable: true, Default: nil, MaxLength: 256},
		{Path: "/skill_evolution/learning/external_context_policy", Type: "string", Default: "hints_only", Enum: []string{"hints_only"}},
		numeric("skill_evolution/learning", "idle_seconds", "integer", 300, 1, 86400),
		numeric("skill_evolution/learning", "timeout_seconds", "integer", 60, 1, 300),
		numeric("skill_evolution/learning", "max_input_bytes", "integer", 131072, 1024, 2<<20),
		numeric("skill_evolution/learning", "max_output_tokens", "integer", 4096, 128, 32768),
		numeric("skill_evolution/learning", "max_daily_model_calls", "integer", 10, 1, 100000),
		numeric("skill_evolution/learning", "max_daily_tokens", "integer", 50000, 1, 1000000000),
		{Path: "/skill_evolution/discovery", Type: "discovery_settings", Default: map[string]any{
			"operator_corrections_enabled": false, "repeat_threshold": 2, "repeat_window_days": 30,
			"missing_reports_enabled": false, "successful_workflows_enabled": false, "experience_patterns_enabled": false, "experience_new_skills_enabled": false, "experience_review_drafts_enabled": false, "new_skill_source_id": "",
			"cooldown_hours": 24, "max_daily_proposals": 5, "max_pending_proposals": 20,
		}},
		{Path: "/skill_evolution/discovery/operator_corrections_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/missing_reports_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/successful_workflows_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/experience_patterns_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/experience_new_skills_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/experience_review_drafts_enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/discovery/new_skill_source_id", Type: "string", Default: "", MaxLength: 128},
		numeric("skill_evolution/discovery", "repeat_threshold", "integer", 2, 2, 16),
		numeric("skill_evolution/discovery", "repeat_window_days", "integer", 30, 1, 365),
		numeric("skill_evolution/discovery", "cooldown_hours", "integer", 24, 1, 8760),
		numeric("skill_evolution/discovery", "max_daily_proposals", "integer", 5, 1, 100),
		numeric("skill_evolution/discovery", "max_pending_proposals", "integer", 20, 1, 1000),
		{Path: "/skill_evolution/behavior", Type: "behavior_settings", Default: map[string]any{
			"enabled": false, "suite_path": "", "timeout_seconds": 60, "max_steps": 6,
			"max_input_bytes": 131072, "max_output_tokens": 1024,
		}},
		{Path: "/skill_evolution/behavior/enabled", Type: "boolean", Default: false},
		{Path: "/skill_evolution/behavior/suite_path", Type: "string", Default: "", MaxLength: 4096},
		numeric("skill_evolution/behavior", "timeout_seconds", "integer", 60, 1, 300),
		numeric("skill_evolution/behavior", "max_steps", "integer", 6, 1, 32),
		numeric("skill_evolution/behavior", "max_input_bytes", "integer", 131072, 1024, 2<<20),
		numeric("skill_evolution/behavior", "max_output_tokens", "integer", 1024, 128, 8192),
		{Path: "/skill_evolution/auto_patch", Type: "auto_patch_settings", Default: map[string]any{
			"experience_patterns_enabled": false,
			"max_changed_lines":           40, "max_daily_publications": 2, "observation_hours": 72,
			"min_observations": 3, "rollback_failure_threshold": 2,
		}},
		{Path: "/skill_evolution/auto_patch/experience_patterns_enabled", Type: "boolean", Default: false},
		numeric("skill_evolution/auto_patch", "max_changed_lines", "integer", 40, 1, 200),
		numeric("skill_evolution/auto_patch", "max_daily_publications", "integer", 2, 1, 20),
		numeric("skill_evolution/auto_patch", "observation_hours", "integer", 72, 1, 720),
		numeric("skill_evolution/auto_patch", "min_observations", "integer", 3, 1, 100),
		numeric("skill_evolution/auto_patch", "rollback_failure_threshold", "integer", 2, 1, 100),
		{Path: "/skill_evolution/budget", Type: "maintenance_budget", Default: map[string]any{
			"max_daily_model_calls": 30, "max_daily_tokens": 100000, "max_job_seconds": 300, "workers": 1,
		}},
		numeric("skill_evolution/budget", "max_daily_model_calls", "integer", 30, 1, 100000),
		numeric("skill_evolution/budget", "max_daily_tokens", "integer", 100000, 1, 1000000000),
		numeric("skill_evolution/budget", "max_job_seconds", "integer", 300, 1, 3600),
		numeric("skill_evolution/budget", "workers", "integer", 1, 1, 16),
	}
}

func generationLimit(key string, min, max float64) Field {
	field := numeric("skill_evolution/generation", key, "integer", 0, min, max)
	field.ZeroIsDefault = true
	return field
}

func reviewLimit(key string, min, max float64) Field {
	field := numeric("skill_evolution/validation", key, "integer", 0, min, max)
	field.ZeroIsDefault = true
	return field
}

// ProjectEvolution exposes consumed fields only. Unsupported original fields
// remain on disk; the paths are enough to explain the explicit conversion.
func ProjectEvolution(raw map[string]any) (map[string]any, []string, error) {
	result := Defaults("skill_evolution")
	var ignored []string
	for key, value := range raw {
		if _, known := result[key]; !known {
			ignored = append(ignored, "skill_evolution."+key)
			continue
		}
		if value == nil {
			continue
		}
		limits, nested := result[key].(map[string]any)
		if !nested {
			result[key] = value
			continue
		}
		values, ok := value.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("skill_evolution.%s: must be an object", key)
		}
		for name, item := range values {
			if _, known := limits[name]; !known {
				ignored = append(ignored, "skill_evolution."+key+"."+name)
			} else if item != nil || key == "discovery" || key == "behavior" || key == "auto_patch" && name == "experience_patterns_enabled" || key == "learning" && name != "extraction_model_name" && name != "consolidation_model_name" && name != "matching_model_name" {
				limits[name] = item
			}
		}
	}
	if _, ok := result["enabled"].(bool); !ok {
		return nil, nil, fmt.Errorf("skill_evolution.enabled: must be a boolean")
	}
	if result["mode"] == "" {
		result["mode"] = "review"
	}
	slices.Sort(ignored)
	return result, ignored, nil
}

// An unchanged disabled legacy value is preserved during unrelated saves.
// New edits and enabled configurations must satisfy the native field contract.
func ValidateEvolutionDraft(document, dormant map[string]any) error {
	enabled, ok := document["enabled"].(bool)
	if !ok {
		return fmt.Errorf("skill_evolution.enabled: must be a boolean")
	}
	check := func(path string, value, original any) error {
		if err := ValidateValue(path, value); err != nil {
			if !enabled && dormant != nil && SameValue(value, original) {
				return nil
			}
			return err
		}
		return nil
	}
	defaults := Defaults("skill_evolution")
	for key, value := range document {
		if _, known := defaults[key]; !known {
			return fmt.Errorf("skill_evolution.%s: unsupported setting", key)
		}
		if err := check("/skill_evolution/"+key, value, dormant[key]); err != nil {
			return err
		}
	}
	for key := range defaults {
		if _, exists := document[key]; !exists {
			return fmt.Errorf("skill_evolution.%s: missing setting", key)
		}
	}
	for _, group := range []string{"validation", "generation", "learning", "discovery", "behavior", "auto_patch", "budget"} {
		values, ok := document[group].(map[string]any)
		if !ok {
			return fmt.Errorf("skill_evolution.%s: must be an object", group)
		}
		previous, _ := dormant[group].(map[string]any)
		known := defaults[group].(map[string]any)
		for key, value := range values {
			path := "/skill_evolution/" + group + "/" + key
			if _, ok := known[key]; !ok {
				return fmt.Errorf("%s: unsupported setting", path)
			}
			if err := check(path, value, previous[key]); err != nil {
				return err
			}
		}
		for key := range known {
			if _, exists := values[key]; !exists {
				// Older desktop drafts predate this optional transport hint. Saving
				// them preserves any on-disk choice, or the text default.
				if group == "validation" && key == "response_format" {
					continue
				}
				return fmt.Errorf("skill_evolution.%s.%s: missing setting", group, key)
			}
		}
	}
	return ValidateEvolutionWorkerPrerequisites(document)
}

// ValidateEvolutionWorkerPrerequisites checks native paid-work opt-ins even
// when the surrounding document otherwise retains disabled legacy data.
func ValidateEvolutionWorkerPrerequisites(document map[string]any) error {
	if err := ValidateEvolutionLearningPrerequisites(document); err != nil {
		return err
	}
	if err := ValidateEvolutionDiscoveryPrerequisites(document); err != nil {
		return err
	}
	if err := validateLearningAutoPatchPrerequisites(document); err != nil {
		return err
	}
	behavior, _ := document["behavior"].(map[string]any)
	if document["enabled"] == true && document["mode"] == "auto_patch" && behavior["enabled"] != true {
		return fmt.Errorf("skill_evolution.mode auto_patch requires behavior.enabled and a host-defined suite")
	}
	value, present := behavior["enabled"]
	if !present {
		return nil
	}
	if err := ValidateValue("/skill_evolution/behavior/enabled", value); err != nil {
		return err
	}
	if value == false {
		return nil
	}
	if document["enabled"] != true {
		return fmt.Errorf("skill_evolution.behavior.enabled requires skill_evolution.enabled")
	}
	generation, _ := document["generation"].(map[string]any)
	if generation["enabled"] != true {
		return fmt.Errorf("skill_evolution.behavior.enabled requires skill_evolution.generation.enabled")
	}
	suite, ok := behavior["suite_path"].(string)
	if !ok || strings.TrimSpace(suite) == "" {
		return fmt.Errorf("skill_evolution.behavior.suite_path: requires a host-defined suite file")
	}
	return nil
}

// Existing automatic publication grants must not silently cover learned
// procedures. This gate is native, independent and disabled by default.
func validateLearningAutoPatchPrerequisites(document map[string]any) error {
	patch, _ := document["auto_patch"].(map[string]any)
	value, present := patch["experience_patterns_enabled"]
	if !present {
		return nil
	}
	if err := ValidateValue("/skill_evolution/auto_patch/experience_patterns_enabled", value); err != nil {
		return err
	}
	if value == false {
		return nil
	}
	discovery, _ := document["discovery"].(map[string]any)
	behavior, _ := document["behavior"].(map[string]any)
	if document["enabled"] != true || document["mode"] != "auto_patch" || discovery["experience_patterns_enabled"] != true || behavior["enabled"] != true {
		return fmt.Errorf("skill_evolution.auto_patch.experience_patterns_enabled requires enabled auto_patch mode, experience discovery and host behavior evaluation")
	}
	return nil
}

// Learning is an independent paid-work opt-in. It does not grant candidate
// generation or publication and cannot be activated by legacy discovery keys.
func ValidateEvolutionLearningPrerequisites(document map[string]any) error {
	learning, _ := document["learning"].(map[string]any)
	if shared, exists := learning["cross_session_enabled"]; exists {
		if err := ValidateValue("/skill_evolution/learning/cross_session_enabled", shared); err != nil {
			return err
		}
		if shared == true && (learning["enabled"] != true || learning["consolidation_enabled"] != true) {
			return fmt.Errorf("skill_evolution.learning.cross_session_enabled requires learning.enabled and consolidation_enabled")
		}
	}
	if matching, exists := learning["matching_enabled"]; exists {
		if err := ValidateValue("/skill_evolution/learning/matching_enabled", matching); err != nil {
			return err
		}
		if matching == true && (learning["enabled"] != true || learning["consolidation_enabled"] != true) {
			return fmt.Errorf("skill_evolution.learning.matching_enabled requires learning.enabled and consolidation_enabled")
		}
	}
	if consolidate, exists := learning["consolidation_enabled"]; exists {
		if err := ValidateValue("/skill_evolution/learning/consolidation_enabled", consolidate); err != nil {
			return err
		}
		if consolidate == true && learning["enabled"] != true {
			return fmt.Errorf("skill_evolution.learning.consolidation_enabled requires learning.enabled")
		}
	}
	value, present := learning["enabled"]
	if !present {
		return nil
	}
	if err := ValidateValue("/skill_evolution/learning/enabled", value); err != nil {
		return err
	}
	if value == false {
		return nil
	}
	if document["enabled"] != true {
		return fmt.Errorf("skill_evolution.learning.enabled requires skill_evolution.enabled")
	}
	for key, item := range learning {
		if err := ValidateValue("/skill_evolution/learning/"+key, item); err != nil {
			return err
		}
	}
	defaults := Defaults("skill_evolution")["learning"].(map[string]any)
	budget, _ := document["budget"].(map[string]any)
	for _, key := range []string{"max_daily_model_calls", "max_daily_tokens", "max_job_seconds", "workers"} {
		if err := ValidateValue("/skill_evolution/budget/"+key, budget[key]); err != nil {
			return err
		}
	}
	for _, key := range []string{"max_daily_model_calls", "max_daily_tokens"} {
		value, present := learning[key]
		if !present {
			value = defaults[key]
		}
		calls, ok := evolutionNumber(value)
		total, totalOK := evolutionNumber(budget[key])
		if !ok || !totalOK || calls > total {
			return fmt.Errorf("skill_evolution.learning.%s must fit the shared maintenance budget", key)
		}
	}
	return nil
}

func evolutionNumber(value any) (float64, bool) {
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" {
		return 0, false
	}
	var number float64
	err = json.Unmarshal(data, &number)
	return number, err == nil
}

// ValidateEvolutionDiscoveryPrerequisites checks the new paid-worker opt-in
// even in an otherwise dormant document. Legacy discovery flags have no effect.
func ValidateEvolutionDiscoveryPrerequisites(document map[string]any) error {
	discovery, _ := document["discovery"].(map[string]any)
	for _, key := range []string{"operator_corrections_enabled", "missing_reports_enabled", "successful_workflows_enabled", "experience_patterns_enabled", "experience_new_skills_enabled", "experience_review_drafts_enabled"} {
		value, present := discovery[key]
		if !present {
			continue
		}
		if err := ValidateValue("/skill_evolution/discovery/"+key, value); err != nil {
			return err
		}
		if value == false {
			continue
		}
		if document["enabled"] != true {
			return fmt.Errorf("skill_evolution.discovery.%s requires skill_evolution.enabled", key)
		}
		generation, _ := document["generation"].(map[string]any)
		if generation["enabled"] != true {
			return fmt.Errorf("skill_evolution.discovery.%s requires skill_evolution.generation.enabled", key)
		}
		if key == "experience_patterns_enabled" {
			learning, _ := document["learning"].(map[string]any)
			if learning["enabled"] != true || learning["consolidation_enabled"] != true || learning["matching_enabled"] != true {
				return fmt.Errorf("skill_evolution.discovery.experience_patterns_enabled requires learning.enabled, consolidation_enabled and matching_enabled")
			}
		}
		if key == "experience_new_skills_enabled" && discovery["experience_patterns_enabled"] != true {
			return fmt.Errorf("skill_evolution.discovery.experience_new_skills_enabled requires experience_patterns_enabled")
		}
		if key == "experience_review_drafts_enabled" && discovery["experience_patterns_enabled"] != true {
			return fmt.Errorf("skill_evolution.discovery.experience_review_drafts_enabled requires experience_patterns_enabled")
		}
		if key == "missing_reports_enabled" || key == "successful_workflows_enabled" || key == "experience_new_skills_enabled" {
			id, ok := discovery["new_skill_source_id"].(string)
			if !ok || strings.TrimSpace(id) == "" {
				return fmt.Errorf("skill_evolution.discovery.new_skill_source_id requires an authorized non-builtin source")
			}
		}
	}
	return nil
}

// JSON/YAML integer representations differ after the desktop round trip.
func SameValue(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func validateNativeEvolution(document map[string]any) error {
	raw, ok := document["skill_evolution"].(map[string]any)
	if !ok {
		if document["skill_evolution"] != nil {
			return fmt.Errorf("skill_evolution: must be an object")
		}
		return nil
	}
	if err := ValidateEvolutionLearningPrerequisites(raw); err != nil {
		return err
	}
	projection, ignored, err := ProjectEvolution(raw)
	if err != nil {
		return err
	}
	if err := ValidateEvolutionWorkerPrerequisites(projection); err != nil {
		return err
	}
	if projection["enabled"] == false {
		return nil // Dormant legacy data is not active policy.
	}
	if len(ignored) != 0 {
		return fmt.Errorf("%s: unsupported setting", ignored[0])
	}
	return ValidateEvolutionDraft(projection, nil)
}

func isEvolutionField(field Field) bool {
	return strings.HasPrefix(field.Path, "/skill_evolution/")
}
