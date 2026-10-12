package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type sdkWorkspaceConsolidator struct{ calls atomic.Int32 }

func (m *sdkWorkspaceConsolidator) Identity() string { return "sdk-workspace-consolidation-v1" }
func (m *sdkWorkspaceConsolidator) Consolidate(ctx context.Context, input harness.SkillExperienceConsolidationInput) (harness.SkillExperienceConsolidationDecision, error) {
	if err := sdkMatchingCall(ctx, "experience_consolidate"); err != nil {
		return harness.SkillExperienceConsolidationDecision{}, err
	}
	m.calls.Add(1)
	if len(input.Patterns) > 0 {
		return harness.SkillExperienceConsolidationDecision{Relation: "equivalent", PatternID: input.Patterns[0].ID, Reason: "Same bounded procedure and environment"}, nil
	}
	return harness.SkillExperienceConsolidationDecision{Relation: "distinct", Reason: "First authorized procedure"}, nil
}

func TestSkillLearningWorkspaceSDKManagementConsentLineageAndDispatcher(t *testing.T) {
	for _, shared := range []bool{false, true} {
		name := "learning-only"
		if shared {
			name = "generation-shared"
		}
		t.Run(name, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 12, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
			cfg.SkillLearning = &harness.SkillLearningOptions{CrossSession: true, Idle: time.Second, Budget: budget, MaxDailyModelCalls: 10, MaxDailyTokens: 50000, Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
			extractor, consolidator, matcher := &sdkMatchingExtractor{}, &sdkWorkspaceConsolidator{}, &sdkExperienceMatcher{}
			cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = extractor, consolidator, matcher
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
					event := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolName: "read_file", ToolCallID: "read-complete-json", Status: "pending"}
					if kind == "tool_start" {
						event.Arguments = []byte(`{"path":"fixture.json"}`)
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
			if shared {
				route := harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: "http://127.0.0.1:1", APIKey: "fixture-unused"}
				var err error
				cfg.SkillValidator, err = NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
				if err != nil {
					t.Fatal(err)
				}
				cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route}, Budget: budget, Sources: []string{"project"}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
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
			if _, err = c.WorkspaceSkillExperiencePatterns(ctx, x.ID, "", 10); err == nil {
				t.Fatal("SDK workspace query implicitly grants session consent")
			}
			share := true
			first, err := c.SetSkillLearningControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: true, WorkspaceShared: &share})
			if err != nil || first.Epoch != 1 || !first.WorkspaceShared {
				t.Fatal(first, err)
			}
			wait := func(revision int64) harness.SkillExperiencePattern {
				t.Helper()
				deadline := time.Now().Add(8 * time.Second)
				for time.Now().Before(deadline) {
					page, err := c.WorkspaceSkillExperiencePatterns(ctx, x.ID, "", 10)
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Patterns) == 1 && page.Patterns[0].Revision == revision {
						matches, err := c.WorkspaceSkillExperienceMatches(ctx, x.ID, "", 10)
						if err != nil {
							t.Fatal(err)
						}
						for _, match := range matches.Matches {
							if match.PatternRevision == revision {
								return page.Patterns[0]
							}
						}
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatal("workspace pipeline did not settle")
				return harness.SkillExperiencePattern{}
			}
			if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "First full JSON task"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			p := wait(1)
			y, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.set", "session_id": y.ID, "control": map[string]any{"contribute": true, "expected_epoch": 0, "workspace_shared": true}}); err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(ctx, y.ID, []harness.Content{{Type: "text", Text: "Second independent full JSON task"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			p = wait(2)
			if p.SupportEpisodes != 2 || p.ObservedEpisodes != 2 || p.EvidenceState != "observed_execution" || p.Quality != "unassessed" {
				t.Fatal("shared dispatcher inflated or lost evidence", p)
			}
			for _, method := range []string{"skill.learning.workspace.patterns", "skill.learning.workspace.matches"} {
				if _, err = skillManagementCall(t, c, map[string]any{"operation": method, "session_id": y.ID, "limit": 10}); err != nil {
					t.Fatal(err)
				}
			}
			z, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			third, err := c.SetSkillLearningControl(ctx, z.ID, harness.SkillLearningControlRequest{Contribute: true, WorkspaceShared: &share})
			if err != nil {
				t.Fatal(err)
			}
			var sourceEpisode string
			if err = c.store.DB().QueryRow(`SELECT id FROM df_learning_episodes WHERE session_id=?`, x.ID).Scan(&sourceEpisode); err != nil {
				t.Fatal(err)
			}
			lineage := map[string]any{"operation": "skill.learning.lineage.link", "session_id": z.ID, "source_session_id": x.ID, "source_episode_id": sourceEpisode, "expected_epoch": third.Epoch, "expected_source_epoch": first.Epoch}
			if _, err = skillManagementCall(t, c, lineage); err != nil {
				t.Fatal(err)
			}
			if _, err = c.LinkSkillLearningEpisode(ctx, z.ID, harness.SkillLearningLineageRequest{SourceSessionID: x.ID, SourceEpisodeID: sourceEpisode, ExpectedEpoch: third.Epoch, ExpectedSourceEpoch: first.Epoch}); !errors.Is(err, harness.ErrSkillConflict) {
				t.Fatal("SDK rebound immutable copied lineage", err)
			}
			for _, control := range []string{
				`{"contribute":true,"expected_epoch":1,"workspace_shared":null}`,
				`{"contribute":true,"expected_epoch":1,"workspace_shared":"true"}`,
				`{"contribute":true,"expected_epoch":1,"workspace_shared":true,"workspace_shared":false}`,
				`{"contribute":true,"expected_epoch":1,"Workspace_Shared":true}`,
			} {
				if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.set", "session_id": y.ID, "control": json.RawMessage(control)}); err == nil {
					t.Fatal("invalid sharing control accepted", control)
				}
			}
			ordinary, err := c.SkillExperiencePatterns(ctx, x.ID, "", 10)
			if err != nil || len(ordinary.Patterns) != 0 {
				t.Fatal("SDK ordinary history leaked shared recipes", ordinary, err)
			}
			var generated int
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs WHERE owner_kind='skill'`).Scan(&generated); err != nil || generated != 0 {
				t.Fatal("workspace aggregation generated a skill", generated, err)
			}
			if consolidator.calls.Load() != 2 || matcher.calls.Load() != 2 || extractor.calls.Load() != 2 {
				t.Fatal("workspace calls do not share bounded stages")
			}
			status, err := c.SkillLearningStatus(ctx)
			if err != nil || !status.CrossSessionEnabled || status.Mode != "shadow" {
				t.Fatal(status, err)
			}
		})
	}
}
