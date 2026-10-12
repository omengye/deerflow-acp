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

type sdkMatchingExtractor struct{ calls atomic.Int32 }

func (e *sdkMatchingExtractor) Identity() string { return "sdk-matching-extract-v1" }
func (e *sdkMatchingExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	if err := sdkMatchingCall(ctx, "experience_extract"); err != nil {
		return nil, err
	}
	e.calls.Add(1)
	refs := []string{}
	for _, ref := range input.References {
		if ref.Kind == "user" || ref.Kind == "tool_observation" && ref.ToolName == "read_file" && ref.State == "completed" {
			refs = append(refs, ref.ID)
		}
	}
	return []harness.SkillExperienceCandidate{{Kind: "proven_recipe", Goal: "Inspect full structured source", Applicability: "Project JSON input", Resolution: "Read complete source and verify fields", Steps: []string{"Read complete text"}, Verification: "Complete local receipt", Outcome: "observed_execution", References: refs}}, nil
}

type sdkExperienceMatcher struct{ calls atomic.Int32 }

func (e *sdkExperienceMatcher) Identity() string { return "sdk-match-v1" }
func (e *sdkExperienceMatcher) Match(ctx context.Context, input harness.SkillExperienceMatchInput) (harness.SkillExperienceMatchDecision, error) {
	if err := sdkMatchingCall(ctx, "skill_match"); err != nil {
		return harness.SkillExperienceMatchDecision{}, err
	}
	e.calls.Add(1)
	if len(input.Skills) == 0 {
		return harness.SkillExperienceMatchDecision{Relation: "new_capability", Reason: "No authorized skill exists"}, nil
	}
	return harness.SkillExperienceMatchDecision{Relation: "body_gap", SkillID: input.Skills[0].ID, Reason: "Missing full structured source instruction"}, nil
}

func sdkMatchingCall(ctx context.Context, stage string) error {
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return harness.ErrSkillValidation
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: stage, ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return err
	}
	return b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true})
}

func TestSkillLearningMatchingSDKLifecycleSharedDispatcherAndBudget(t *testing.T) {
	for _, mode := range []string{"learning-only", "generation-shared", "learning-cap", "tools-denied"} {
		t.Run(mode, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			extractor, consolidator, matcher := &sdkMatchingExtractor{}, &sdkExperienceConsolidator{}, &sdkExperienceMatcher{}
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 6, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
			cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 3, MaxDailyTokens: 50000, Budget: budget, Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
			if mode == "learning-cap" {
				cfg.SkillLearning.MaxDailyModelCalls = 2
			}
			if mode == "tools-denied" {
				cfg.ToolPolicy.Denylist = []string{"skill", "read_skill_file"}
			}
			cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = extractor, consolidator, matcher
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
					event := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolName: "read_file", ToolCallID: "read-full-json", Status: "pending"}
					if kind == "tool_start" {
						event.Arguments = []byte(`{"path":"input.json"}`)
					} else if kind == "tool_execute" {
						event.Status = "in_progress"
					} else {
						event.Status, event.Content = "completed", []harness.Content{{Type: "text", Text: `{"verified":true}`}}
					}
					if err := emit(ctx, event); err != nil {
						return harness.RunResult{}, err
					}
				}
				return harness.RunResult{StopReason: "end_turn"}, nil
			})
			if mode == "generation-shared" {
				route := harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: "http://127.0.0.1:1", APIKey: "fixture-not-used"}
				var err error
				cfg.SkillValidator, err = NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
				if err != nil {
					t.Fatal(err)
				}
				cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route}, Budget: budget, Sources: []string{"project"}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Inspect full JSON source"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			var jobState, jobCode string
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				_ = c.store.DB().QueryRow(`SELECT j.state,j.error_code FROM df_learning_matches m JOIN df_skill_maintenance_jobs j ON j.id=m.job_id WHERE m.session_id=?`, x.ID).Scan(&jobState, &jobCode)
				if jobState == "completed" || jobState == "failed" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			page, err := c.SkillExperienceMatches(ctx, x.ID, "", 10)
			want, wantCalls := 1, int32(1)
			if mode == "learning-cap" {
				want, wantCalls = 0, 0
				if jobCode != "budget_exhausted" {
					t.Fatal("matching bypassed shared learning cap", jobState, jobCode)
				}
			}
			if err != nil || len(page.Matches) != want || matcher.calls.Load() != wantCalls || extractor.calls.Load() != 1 || consolidator.calls.Load() != 1 {
				var pending string
				_ = c.store.DB().QueryRow(`SELECT group_concat(state||':'||reason) FROM df_learning_match_inputs`).Scan(&pending)
				t.Fatal("matching pipeline did not finish", page, jobState, jobCode, pending, err)
			}
			if mode == "tools-denied" && page.Matches[0].Target != nil {
				t.Fatal("tool policy denial still exposed a skill", page)
			}
			if mode == "learning-only" || mode == "generation-shared" {
				m := page.Matches[0]
				if m.Target == nil || m.Target.SourceID != "project" || m.Target.Name != "research" || len(m.ActualReads) != 0 || m.SourceState != "not_revalidated" {
					t.Fatal("host-fixed identity or read attribution lost", m)
				}
			}
			if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.matches", "session_id": x.ID, "limit": 10}); err != nil {
				t.Fatal(err)
			}
			if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.matches", "session_id": x.ID, "target_source": "foreign"}); err == nil {
				t.Fatal("management match query accepted target authority")
			}
			var generated, calls int
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs WHERE owner_kind='skill'`).Scan(&generated); err != nil || generated != 0 {
				t.Fatal("matching created skill jobs", generated, err)
			}
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&calls); err != nil || calls != 2+int(wantCalls) {
				t.Fatal("shared producer accounting incorrect", calls, err)
			}
			if _, err = c.SetSkillLearningControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1}); err != nil {
				t.Fatal(err)
			}
			page, err = c.SkillExperienceMatches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != 0 {
				t.Fatal("opt-out retained matching text", page, err)
			}
		})
	}
}

func TestSkillLearningMatchingSDKRejectsAdapterWithoutOptIn(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), SkillExperienceMatcher: &sdkExperienceMatcher{}}
	if c, err := Open(context.Background(), cfg); err == nil {
		_ = c.Close()
		t.Fatal("matcher activated paid work without opt-in")
	}
}
