package evolution

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// ReviewSettings contains versioned host policy for the automatic pipeline.
// It is not a customer configuration surface.
type ReviewSettings struct {
	PrimaryModel         string
	AllowBuiltinOverlays bool
	Validation           struct {
		ResponseFormat  string
		TimeoutSeconds  int
		MaxInputBytes   int
		MaxOutputTokens int
	}
	Generation struct {
		TimeoutSeconds  int
		MaxInputBytes   int
		MaxOutputTokens int
	}
	Learning struct {
		IdleSeconds     int
		TimeoutSeconds  int
		MaxInputBytes   int
		MaxOutputTokens int
	}
	Discovery struct {
		DestinationSourceID string
		RepeatThreshold     int
		RepeatWindowDays    int
		CooldownHours       int
		MaxPendingProposals int
	}
	Policy struct {
		MaxJobSeconds int64
		Workers       int
	}
}

// Only the primary model is used. Retired configuration is rejected after migration.
func DecodeReviewConfig(data []byte, models map[string]bool, defaultModel string) (ReviewSettings, error) {
	var raw map[string]any
	if len(bytes.TrimSpace(data)) != 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return ReviewSettings{}, err
		}
		if len(raw) != 0 {
			return ReviewSettings{}, fmt.Errorf("skill_evolution configuration has been removed; migrate the configuration before startup")
		}
	}
	if defaultModel == "" || !models[defaultModel] {
		return ReviewSettings{}, fmt.Errorf("automatic skill evolution requires a configured primary model")
	}
	return AutomaticSettings(defaultModel), nil
}

// AutomaticSettings is host policy. It is never decoded from customer YAML.
func AutomaticSettings(model string) ReviewSettings {
	var s ReviewSettings
	s.PrimaryModel = model
	s.Validation.TimeoutSeconds, s.Validation.MaxInputBytes, s.Validation.MaxOutputTokens = 120, 256<<10, 2048
	s.Validation.ResponseFormat = "text"
	s.Generation.TimeoutSeconds, s.Generation.MaxInputBytes, s.Generation.MaxOutputTokens = 120, 256<<10, 8192
	s.Learning.IdleSeconds, s.Learning.TimeoutSeconds = 30, 120
	s.Learning.MaxInputBytes, s.Learning.MaxOutputTokens = 256<<10, 4096
	s.Discovery.RepeatThreshold, s.Discovery.RepeatWindowDays = 2, 30
	s.Discovery.CooldownHours, s.Discovery.MaxPendingProposals = 24, 20
	s.Policy.MaxJobSeconds, s.Policy.Workers = 900, 2
	return s
}

func (s ReviewSettings) LearningOptions(route harness.ModelRoute) *harness.SkillLearningOptions {
	return &harness.SkillLearningOptions{
		Model:        harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens},
		Policy:       harness.SkillMaintenancePolicy{MaxJobSeconds: s.Policy.MaxJobSeconds, Workers: s.Policy.Workers},
		CrossSession: true,
		Idle:         time.Duration(s.Learning.IdleSeconds) * time.Second}
}

func (s ReviewSettings) LearningConsolidationModel(route harness.ModelRoute) *harness.SkillGenerationModelConfig {
	return &harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens}
}

func (s ReviewSettings) LearningMatchingModel(route harness.ModelRoute) *harness.SkillGenerationModelConfig {
	return &harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Learning.TimeoutSeconds) * time.Second, MaxInputBytes: s.Learning.MaxInputBytes, MaxOutputTokens: s.Learning.MaxOutputTokens}
}

func (s ReviewSettings) ModelConfig(moderation harness.ModelRoute) harness.SkillModelReviewConfig {
	return harness.SkillModelReviewConfig{Moderation: moderation, Timeout: time.Duration(s.Validation.TimeoutSeconds) * time.Second, MaxInputBytes: s.Validation.MaxInputBytes, MaxOutputTokens: s.Validation.MaxOutputTokens, ResponseFormat: s.Validation.ResponseFormat}
}

func (s ReviewSettings) GenerationOptions(route harness.ModelRoute, sources []string) *harness.SkillGenerationOptions {
	return &harness.SkillGenerationOptions{
		Model:   harness.SkillGenerationModelConfig{Route: route, Timeout: time.Duration(s.Generation.TimeoutSeconds) * time.Second, MaxInputBytes: s.Generation.MaxInputBytes, MaxOutputTokens: s.Generation.MaxOutputTokens},
		Policy:  harness.SkillMaintenancePolicy{MaxJobSeconds: s.Policy.MaxJobSeconds, Workers: s.Policy.Workers},
		Sources: slices.Clone(sources), AllowBuiltinOverlays: s.AllowBuiltinOverlays, PublishAfterSecurityReview: true,
	}
}

func (s ReviewSettings) DiscoveryOptions() *harness.SkillDiscoveryOptions {
	return &harness.SkillDiscoveryOptions{
		DestinationSourceID: s.Discovery.DestinationSourceID,
		RepeatThreshold:     s.Discovery.RepeatThreshold, RepeatWindowDays: s.Discovery.RepeatWindowDays,
		CooldownHours: s.Discovery.CooldownHours, MaxPendingProposals: s.Discovery.MaxPendingProposals,
	}
}
