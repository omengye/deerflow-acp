package deerflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

type sdkAutoPatchLearningEvaluator struct{}

func (*sdkAutoPatchLearningEvaluator) Identity() string { return "sdk-learning-auto-patch-fixture-v1" }
func (*sdkAutoPatchLearningEvaluator) Evaluate(context.Context, harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	return harness.SkillBehaviorResult{}, harness.ErrPermissionDenied // No job is queued by this startup/status fixture.
}

func TestSkillLearningAutoPatchSDKIndependentStartupGrantAndManagementStatus(t *testing.T) {
	for _, mode := range []string{"on", "old-grant", "no-discovery", "no-learning", "no-matching"} {
		t.Run(mode, func(t *testing.T) {
			_, cfg := skillFixture(t)
			cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
				return harness.RunResult{StopReason: "end_turn"}, nil
			})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "no model call authorized by this fixture", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 20, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
			cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 10, MaxDailyTokens: 50000, Budget: budget, Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
			cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = &sdkMatchingExtractor{}, &sdkExperienceMerge{}, &sdkExperienceMatcher{}
			cfg.SkillValidator = &sdkExperienceReview{}
			cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "unused-fixture"}}, Budget: budget, Sources: []string{"project"}}
			cfg.SkillDiscovery = &harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
			cfg.SkillBehavior = &harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{{Version: "skill-behavior-v1", ID: "research-fixture", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{
				{ID: "targeted", Kind: "targeted", Prompt: "Verify sources", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "output_equals", Expected: "verified"}}},
				{ID: "regression", Kind: "regression", Prompt: "Retain existing behavior", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "output_equals", Expected: "retained"}}},
			}}}}
			cfg.SkillBehaviorEvaluator = &sdkAutoPatchLearningEvaluator{}
			cfg.SkillAutoPatch = &harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: mode != "old-grant", MaxChangedLines: 40, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
			switch mode {
			case "no-discovery":
				cfg.SkillDiscovery = nil
			case "no-learning":
				cfg.SkillLearning, cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = nil, nil, nil, nil
			case "no-matching":
				cfg.SkillLearning.Matching, cfg.SkillExperienceMatcher = nil, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if mode != "on" && mode != "old-grant" {
				if err == nil {
					_ = c.Close()
					t.Fatal("incomplete chain granted automatic learning publication")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			status, err := c.SkillLearningStatus(ctx)
			want := "guarded_auto_patch"
			if mode == "old-grant" {
				want = "review_candidates"
			}
			if err != nil || status.Mode != want || status.AutoPatchEnabled != (mode == "on") || !status.ProposalDiscoveryEnabled {
				t.Fatal("learning status hides independent publication permission", status, err)
			}
			data, err := c.ManageLocal(ctx, localhost.ManagementRequest{Operation: "skill.learning.status"})
			managed, ok := data.(harness.SkillLearningStatus)
			if err != nil || !ok || managed != status {
				t.Fatal("management status differs from trusted SDK", data, status, err)
			}
			patch, err := c.SkillAutoPatchStatus(ctx)
			if err != nil || !patch.Enabled || patch.Options == nil || patch.Options.ExperiencePatternsEnabled != (mode == "on") || patch.PublishedToday != 0 {
				t.Fatal("automatic publication status", patch, err)
			}
			if calls.Load() != 0 {
				t.Fatal("startup/status unexpectedly called a model", calls.Load())
			}
		})
	}
}
