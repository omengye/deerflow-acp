package evolution

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"go.yaml.in/yaml/v3"
)

// ReviewSettings configures manual candidates, generation and explicit opt-in
// selection of operator corrections. Unsupported discovery/publication fields
// are not silently accepted as effective settings.
// Disabled legacy documents are retained for explicit later migration.
type ReviewSettings struct {
	Enabled              bool   `yaml:"enabled"`
	Mode                 string `yaml:"mode"`
	ModerationModel      string `yaml:"moderation_model_name"`
	EvaluationModel      string `yaml:"evaluation_model_name"`
	GenerationModel      string `yaml:"generation_model_name"`
	SecurityFailClosed   *bool  `yaml:"security_fail_closed"`
	AllowBuiltinOverlays bool   `yaml:"allow_builtin_overlays"`
	Validation           struct {
		ResponseFormat  string `yaml:"response_format"`
		TimeoutSeconds  int    `yaml:"timeout_seconds"`
		MaxInputBytes   int    `yaml:"max_input_bytes"`
		MaxOutputTokens int    `yaml:"max_output_tokens"`
	} `yaml:"validation"`
	Generation struct {
		Enabled         bool `yaml:"enabled"`
		TimeoutSeconds  int  `yaml:"timeout_seconds"`
		MaxInputBytes   int  `yaml:"max_input_bytes"`
		MaxOutputTokens int  `yaml:"max_output_tokens"`
	} `yaml:"generation"`
	Learning struct {
		Enabled               bool   `yaml:"enabled"`
		ExtractionModel       string `yaml:"extraction_model_name"`
		ConsolidationEnabled  bool   `yaml:"consolidation_enabled"`
		ConsolidationModel    string `yaml:"consolidation_model_name"`
		CrossSessionEnabled   bool   `yaml:"cross_session_enabled"`
		MatchingEnabled       bool   `yaml:"matching_enabled"`
		MatchingModel         string `yaml:"matching_model_name"`
		IdleSeconds           int    `yaml:"idle_seconds"`
		TimeoutSeconds        int    `yaml:"timeout_seconds"`
		MaxInputBytes         int    `yaml:"max_input_bytes"`
		MaxOutputTokens       int    `yaml:"max_output_tokens"`
		MaxDailyModelCalls    int64  `yaml:"max_daily_model_calls"`
		MaxDailyTokens        int64  `yaml:"max_daily_tokens"`
		ExternalContextPolicy string `yaml:"external_context_policy"`
	} `yaml:"learning"`
	Discovery struct {
		OperatorCorrectionsEnabled    bool   `yaml:"operator_corrections_enabled"`
		MissingReportsEnabled         bool   `yaml:"missing_reports_enabled"`
		SuccessfulWorkflowsEnabled    bool   `yaml:"successful_workflows_enabled"`
		ExperiencePatternsEnabled     bool   `yaml:"experience_patterns_enabled"`
		ExperienceNewSkillsEnabled    bool   `yaml:"experience_new_skills_enabled"`
		ExperienceReviewDraftsEnabled bool   `yaml:"experience_review_drafts_enabled"`
		NewSkillSourceID              string `yaml:"new_skill_source_id"`
		RepeatThreshold               int    `yaml:"repeat_threshold"`
		RepeatWindowDays              int    `yaml:"repeat_window_days"`
		CooldownHours                 int    `yaml:"cooldown_hours"`
		MaxDailyProposals             int    `yaml:"max_daily_proposals"`
		MaxPendingProposals           int    `yaml:"max_pending_proposals"`
	} `yaml:"discovery"`
	Behavior struct {
		Enabled         bool   `yaml:"enabled"`
		SuitePath       string `yaml:"suite_path"`
		TimeoutSeconds  int    `yaml:"timeout_seconds"`
		MaxSteps        int    `yaml:"max_steps"`
		MaxInputBytes   int    `yaml:"max_input_bytes"`
		MaxOutputTokens int    `yaml:"max_output_tokens"`
	} `yaml:"behavior"`
	AutoPatch struct {
		ExperiencePatternsEnabled bool `yaml:"experience_patterns_enabled"`
		MaxChangedLines           int  `yaml:"max_changed_lines"`
		MaxDailyPublications      int  `yaml:"max_daily_publications"`
		ObservationHours          int  `yaml:"observation_hours"`
		MinObservations           int  `yaml:"min_observations"`
		RollbackFailureThreshold  int  `yaml:"rollback_failure_threshold"`
	} `yaml:"auto_patch"`
	Budget struct {
		MaxDailyModelCalls int64 `yaml:"max_daily_model_calls"`
		MaxDailyTokens     int64 `yaml:"max_daily_tokens"`
		MaxJobSeconds      int64 `yaml:"max_job_seconds"`
		Workers            int   `yaml:"workers"`
	} `yaml:"budget"`
}

// Omitted, null and empty model references inherit the configured global default.
// Only active stages resolve their references; saved settings stay sparse.
func DecodeReviewConfig(data []byte, models map[string]bool, defaultModel string) (ReviewSettings, error) {
	var settings ReviewSettings
	var enabled struct {
		Enabled bool `yaml:"enabled"`
	}
	if err := yaml.Unmarshal(data, &enabled); err != nil {
		return settings, fmt.Errorf("skill_evolution.enabled: %w", err)
	}
	// Validate numeric shapes before YAML can coerce fractions into integers.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return settings, fmt.Errorf("skill_evolution: %w", err)
	}
	if err := configschema.ValidateEvolutionWorkerPrerequisites(raw); err != nil {
		return settings, err
	}
	if !enabled.Enabled {
		return settings, nil
	}
	projected, _, err := configschema.ProjectEvolution(raw)
	if err != nil {
		return settings, err
	}
	if err := configschema.ValidateEvolutionDraft(projected, nil); err != nil {
		return settings, err
	}
	// Projected defaults apply to omitted numeric settings. Explicit zero is
	// rejected above instead of being interpreted as permission to use defaults.
	discovery := projected["discovery"].(map[string]any)
	discoveryYAML, err := yaml.Marshal(discovery)
	if err != nil {
		return settings, fmt.Errorf("skill_evolution.discovery: %w", err)
	}
	if err := yaml.Unmarshal(discoveryYAML, &settings.Discovery); err != nil {
		return settings, fmt.Errorf("skill_evolution.discovery: %w", err)
	}
	behaviorYAML, err := yaml.Marshal(projected["behavior"])
	if err != nil {
		return settings, fmt.Errorf("skill_evolution.behavior: %w", err)
	}
	if err := yaml.Unmarshal(behaviorYAML, &settings.Behavior); err != nil {
		return settings, fmt.Errorf("skill_evolution.behavior: %w", err)
	}
	learningYAML, err := yaml.Marshal(projected["learning"])
	if err != nil {
		return settings, err
	}
	if err := yaml.Unmarshal(learningYAML, &settings.Learning); err != nil {
		return settings, fmt.Errorf("skill_evolution.learning: %w", err)
	}
	autoPatchYAML, err := yaml.Marshal(projected["auto_patch"])
	if err != nil {
		return settings, err
	}
	if err := yaml.Unmarshal(autoPatchYAML, &settings.AutoPatch); err != nil {
		return settings, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("skill_evolution: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return settings, fmt.Errorf("skill_evolution: expected one configuration document")
	}
	if settings.Mode == "" {
		settings.Mode = "review"
	}
	if settings.Mode != "review" && settings.Mode != "auto_patch" {
		return settings, fmt.Errorf("skill_evolution.mode: must be review or auto_patch")
	}
	if settings.SecurityFailClosed != nil && !*settings.SecurityFailClosed {
		return settings, fmt.Errorf("skill_evolution.security_fail_closed must be true")
	}
	resolveModel := func(key string, name *string) error {
		if *name == "" {
			*name = defaultModel
		}
		if *name == "" || !models[*name] {
			return fmt.Errorf("skill_evolution.%s: requires a configured model name or valid default_model", key)
		}
		return nil
	}
	if err := resolveModel("moderation_model_name", &settings.ModerationModel); err != nil {
		return settings, err
	}
	if err := resolveModel("evaluation_model_name", &settings.EvaluationModel); err != nil {
		return settings, err
	}
	if settings.Validation.TimeoutSeconds == 0 {
		settings.Validation.TimeoutSeconds = 60
	}
	if settings.Validation.MaxInputBytes == 0 {
		settings.Validation.MaxInputBytes = 128 << 10
	}
	if settings.Validation.MaxOutputTokens == 0 {
		settings.Validation.MaxOutputTokens = 1024
	}
	if settings.Validation.TimeoutSeconds < 1 || settings.Validation.TimeoutSeconds > 300 {
		return settings, fmt.Errorf("skill_evolution.validation.timeout_seconds must be 1..300")
	}
	if settings.Validation.MaxInputBytes < 1024 || settings.Validation.MaxInputBytes > 2<<20 {
		return settings, fmt.Errorf("skill_evolution.validation.max_input_bytes must be 1024..2097152")
	}
	if settings.Validation.MaxOutputTokens < 128 || settings.Validation.MaxOutputTokens > 8192 {
		return settings, fmt.Errorf("skill_evolution.validation.max_output_tokens must be 128..8192")
	}
	if settings.Learning.Enabled {
		if err := resolveModel("learning.extraction_model_name", &settings.Learning.ExtractionModel); err != nil {
			return settings, err
		}
		if settings.Learning.ConsolidationEnabled {
			if err := resolveModel("learning.consolidation_model_name", &settings.Learning.ConsolidationModel); err != nil {
				return settings, err
			}
		}
		if settings.Learning.MatchingEnabled {
			if err := resolveModel("learning.matching_model_name", &settings.Learning.MatchingModel); err != nil {
				return settings, err
			}
		}
	}
	if settings.Generation.Enabled {
		if err := resolveModel("generation_model_name", &settings.GenerationModel); err != nil {
			return settings, err
		}
	}
	if settings.Generation.Enabled || settings.Learning.Enabled {
		// Unlike model limits, budget values must be explicitly positive.
		for key, value := range map[string]any{
			"max_daily_model_calls": settings.Budget.MaxDailyModelCalls,
			"max_daily_tokens":      settings.Budget.MaxDailyTokens,
			"max_job_seconds":       settings.Budget.MaxJobSeconds,
			"workers":               settings.Budget.Workers,
		} {
			if err := configschema.ValidateValue("/skill_evolution/budget/"+key, value); err != nil {
				return settings, err
			}
		}
	}
	if (settings.Discovery.MissingReportsEnabled || settings.Discovery.SuccessfulWorkflowsEnabled || settings.Discovery.ExperienceNewSkillsEnabled) && !harness.ValidSkillName(settings.Discovery.NewSkillSourceID) {
		return settings, fmt.Errorf("skill_evolution.discovery.new_skill_source_id: requires a valid source identifier")
	}
	return settings, nil
}

func (s ReviewSettings) LearningOptions(route harness.ModelRoute) *harness.SkillLearningOptions {
	if !s.Enabled || !s.Learning.Enabled {
		return nil
	}
	return &harness.SkillLearningOptions{
		Model:        harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens},
		Budget:       harness.SkillMaintenanceBudget{MaxDailyModelCalls: s.Budget.MaxDailyModelCalls, MaxDailyTokens: s.Budget.MaxDailyTokens, MaxJobSeconds: s.Budget.MaxJobSeconds, Workers: s.Budget.Workers},
		CrossSession: s.Learning.CrossSessionEnabled,
		Idle:         time.Duration(s.Learning.IdleSeconds) * time.Second, MaxDailyModelCalls: s.Learning.MaxDailyModelCalls, MaxDailyTokens: s.Learning.MaxDailyTokens,
	}
}

func (s ReviewSettings) LearningConsolidationModel(route harness.ModelRoute) *harness.SkillGenerationModelConfig {
	if !s.Enabled || !s.Learning.Enabled || !s.Learning.ConsolidationEnabled {
		return nil
	}
	return &harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens}
}

func (s ReviewSettings) LearningMatchingModel(route harness.ModelRoute) *harness.SkillGenerationModelConfig {
	if !s.Enabled || !s.Learning.Enabled || !s.Learning.ConsolidationEnabled || !s.Learning.MatchingEnabled {
		return nil
	}
	return &harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens}
}

func (s ReviewSettings) ModelConfig(moderation, evaluation harness.ModelRoute) harness.SkillModelReviewConfig {
	return harness.SkillModelReviewConfig{Moderation: moderation, Evaluation: evaluation, Timeout: time.Duration(s.Validation.TimeoutSeconds) * time.Second, MaxInputBytes: s.Validation.MaxInputBytes, MaxOutputTokens: s.Validation.MaxOutputTokens, ResponseFormat: s.Validation.ResponseFormat}
}

func (s ReviewSettings) GenerationOptions(route harness.ModelRoute, sources []string) *harness.SkillGenerationOptions {
	if !s.Enabled || !s.Generation.Enabled {
		return nil
	}
	return &harness.SkillGenerationOptions{
		Model:   harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Generation.TimeoutSeconds) * time.Second, MaxInputBytes: s.Generation.MaxInputBytes, MaxOutputTokens: s.Generation.MaxOutputTokens},
		Budget:  harness.SkillMaintenanceBudget{MaxDailyModelCalls: s.Budget.MaxDailyModelCalls, MaxDailyTokens: s.Budget.MaxDailyTokens, MaxJobSeconds: s.Budget.MaxJobSeconds, Workers: s.Budget.Workers},
		Sources: slices.Clone(sources), AllowBuiltinOverlays: s.AllowBuiltinOverlays,
	}
}

func (s ReviewSettings) DiscoveryOptions() *harness.SkillDiscoveryOptions {
	if !s.Enabled || !s.Generation.Enabled || !(s.Discovery.OperatorCorrectionsEnabled || s.Discovery.MissingReportsEnabled || s.Discovery.SuccessfulWorkflowsEnabled || s.Discovery.ExperiencePatternsEnabled) {
		return nil
	}
	return &harness.SkillDiscoveryOptions{
		DisableOperatorCorrections: !s.Discovery.OperatorCorrectionsEnabled,
		MissingReportsEnabled:      s.Discovery.MissingReportsEnabled, SuccessfulWorkflowsEnabled: s.Discovery.SuccessfulWorkflowsEnabled,
		ExperiencePatternsEnabled:     s.Discovery.ExperiencePatternsEnabled,
		ExperienceNewSkillsEnabled:    s.Discovery.ExperienceNewSkillsEnabled,
		ExperienceReviewDraftsEnabled: s.Discovery.ExperienceReviewDraftsEnabled,
		NewSkillSourceID:              s.Discovery.NewSkillSourceID,
		RepeatThreshold:               s.Discovery.RepeatThreshold, RepeatWindowDays: s.Discovery.RepeatWindowDays,
		CooldownHours: s.Discovery.CooldownHours, MaxDailyProposals: s.Discovery.MaxDailyProposals,
		MaxPendingProposals: s.Discovery.MaxPendingProposals,
	}
}

func (s ReviewSettings) AutoPatchOptions() *harness.SkillAutoPatchOptions {
	if !s.Enabled || s.Mode != "auto_patch" {
		return nil
	}
	return &harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: s.AutoPatch.ExperiencePatternsEnabled, MaxChangedLines: s.AutoPatch.MaxChangedLines, MaxDailyPublications: s.AutoPatch.MaxDailyPublications, ObservationHours: s.AutoPatch.ObservationHours, MinObservations: s.AutoPatch.MinObservations, RollbackFailureThreshold: s.AutoPatch.RollbackFailureThreshold}
}

func (s ReviewSettings) BehaviorModelConfig(route harness.ModelRoute) harness.SkillBehaviorModelConfig {
	return harness.SkillBehaviorModelConfig{
		Route: route, Timeout: time.Duration(s.Behavior.TimeoutSeconds) * time.Second,
		MaxInputBytes: s.Behavior.MaxInputBytes, MaxOutputTokens: s.Behavior.MaxOutputTokens,
		MaxModelCalls: s.Behavior.MaxSteps, MaxToolCalls: 24,
	}
}
