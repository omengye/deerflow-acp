package deerflow

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type sdkExperienceConsolidator struct{ calls atomic.Int32 }

func (e *sdkExperienceConsolidator) Identity() string { return "sdk-consolidate-v1" }
func (e *sdkExperienceConsolidator) Consolidate(ctx context.Context, input harness.SkillExperienceConsolidationInput) (harness.SkillExperienceConsolidationDecision, error) {
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return harness.SkillExperienceConsolidationDecision{}, harness.ErrSkillValidation
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: "experience_consolidate", ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return harness.SkillExperienceConsolidationDecision{}, err
	}
	if err = b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
		return harness.SkillExperienceConsolidationDecision{}, err
	}
	e.calls.Add(1)
	return harness.SkillExperienceConsolidationDecision{Relation: "distinct", Reason: "No authorized equivalent pattern"}, nil
}

func TestSkillLearningConsolidationSDKLifecycleManagementAndSharedDispatcher(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "learning-only"
		if shared {
			name = "generation-shared"
		}
		t.Run(name, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			extractor, consolidator := &sdkExperienceExtractor{}, &sdkExperienceConsolidator{}
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 5, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
			cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 3, MaxDailyTokens: 50000, Budget: budget, Consolidation: &harness.SkillGenerationModelConfig{}}
			cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator = extractor, consolidator
			cfg.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				err := emit(ctx, harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "Inspect the complete configuration"})
				return harness.RunResult{StopReason: "end_turn"}, err
			})
			if shared {
				route := harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: "http://127.0.0.1:1", APIKey: "fixture-not-used"}
				var err error
				cfg.SkillValidator, err = NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
				if err != nil {
					t.Fatal(err)
				}
				cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route}, Budget: budget, Sources: []string{"project"}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			x, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Inspect JSON"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			var page harness.SkillExperiencePatternPage
			deadline := time.Now().Add(7 * time.Second)
			for time.Now().Before(deadline) {
				page, err = c.SkillExperiencePatterns(ctx, x.ID, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Patterns) != 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(page.Patterns) != 1 || consolidator.calls.Load() != 1 || extractor.calls.Load() != 1 || page.Patterns[0].EvidenceState != "unresolved" {
				t.Fatal("consolidation worker did not persist shadow result", page)
			}
			status, err := c.SkillLearningStatus(ctx)
			if err != nil || !status.ConsolidationEnabled || status.Patterns != 1 || status.Mode != "shadow" {
				t.Fatal(status, err)
			}
			if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.patterns", "session_id": x.ID, "limit": 10}); err != nil {
				t.Fatal(err)
			}
			if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.patterns", "session_id": x.ID, "operator_confirmed": true}); err == nil {
				t.Fatal("pattern query accepted authority fields")
			}
			var generated int
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs WHERE owner_kind='skill'`).Scan(&generated); err != nil || generated != 0 {
				t.Fatal("shadow consolidation created skill jobs", generated, err)
			}
			if _, err = c.SetSkillLearningControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1}); err != nil {
				t.Fatal(err)
			}
			page, err = c.SkillExperiencePatterns(ctx, x.ID, "", 10)
			if err != nil || len(page.Patterns) != 0 {
				t.Fatal("opt-out retained pattern text", page, err)
			}
		})
	}
}

func TestSkillLearningConsolidationSDKRejectsAdapterWithoutOptIn(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), SkillExperienceConsolidator: &sdkExperienceConsolidator{}}
	if c, err := Open(context.Background(), cfg); err == nil {
		_ = c.Close()
		t.Fatal("adapter enabled paid work without opt-in")
	}
}
