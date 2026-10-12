package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skilllearningtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestSkillLearningNativeDelegatedPipelineAutomaticReviewAndPublication(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
			defer cancel()
			workspace, cfg := skillFixture(t)
			if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"verified":true,"token":"NATIVE_PRIVATE_LOCAL_TOKEN"}`), 0600); err != nil {
				t.Fatal(err)
			}
			provider := skilllearningtest.Delegated(t)
			server := httptest.NewServer(provider)
			defer server.Close()
			route := func(name string) harness.ModelRoute {
				return harness.ModelRoute{Provider: "openai", Model: name, BaseURL: server.URL + "/v1", APIKey: "fixture-only"}
			}
			model := func(name string) harness.SkillGenerationModelConfig {
				return harness.SkillGenerationModelConfig{Route: route(name), Timeout: 10 * time.Second, MaxOutputTokens: 1024}
			}
			cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL = "openai", "foreground", "fixture-only", server.URL+"/v1"
			cfg.DisableSubagents, cfg.BackgroundWorkers = false, 1
			cfg.PermissionMode = harness.PermissionModeOff
			cfg.ToolPolicy.Allowlist = []string{"task", "background_agent", "skill", "read_file"}
			cfg.ModelRoutes = map[string]harness.ModelRoute{"worker": route("child")}
			cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"worker": {Description: "Verify complete local input", SystemPrompt: "Read the complete local JSON document", Model: "worker", Tools: []string{"skill", "read_file"}}}}
			cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 6, MaxTokens: 100000, MaxOutputTokens: 512}
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 20, MaxDailyTokens: 200000, MaxJobSeconds: 60, Workers: 1}
			consolidation, matching := model("consolidate"), model("match")
			cfg.SkillLearning = &harness.SkillLearningOptions{Model: model("extract"), Consolidation: &consolidation, Matching: &matching,
				CrossSession: automatic, Idle: time.Second, Budget: budget, MaxDailyModelCalls: 6, MaxDailyTokens: 100000}
			cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: model("generate"), Sources: []string{"project"}, Budget: budget}
			var err error
			cfg.SkillValidator, err = NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route("security"), Evaluation: route("quality"), MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			cfg.SkillBehaviorEvaluator, err = NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: route("behavior"), MaxModelCalls: 1, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			cfg.SkillBehavior = &harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{{Version: "skill-behavior-v1", ID: "native-delegated-json-check", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{
				{ID: "targeted", Kind: "targeted", Prompt: "Inspect the complete JSON document and report completion.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-delegated-complete", Kind: "output_equals", Expected: "verified"}}},
				{ID: "regression", Kind: "regression", Prompt: "Retain the existing source research behavior.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-delegated-retained", Kind: "output_equals", Expected: "retained"}}},
			}}}}
			cfg.SkillDiscovery = &harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, ExperienceReviewDraftsEnabled: !automatic,
				RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
			if automatic {
				cfg.SkillAutoPatch = &harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: true, MaxChangedLines: 20, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
			}
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			if err = c.skills.WithPublication(ctx, "project", "research", func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err == nil {
					_, err = tx.Publish(head.Version, head, true)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			base, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			wait := func(description string, ready func() bool) {
				t.Helper()
				deadline := time.Now().Add(22 * time.Second)
				for time.Now().Before(deadline) && ctx.Err() == nil {
					if ready() {
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
				var jobs, outbox, background string
				_ = c.store.DB().QueryRow(`SELECT COALESCE(group_concat(job_kind||':'||state||':'||error_code),'') FROM df_skill_maintenance_jobs`).Scan(&jobs)
				_ = c.store.DB().QueryRow(`SELECT COALESCE(group_concat(state||':'||reason),'') FROM harness_learning_outbox`).Scan(&outbox)
				_ = c.store.DB().QueryRow(`SELECT COALESCE(group_concat(status),'') FROM eino_background_tasks`).Scan(&background)
				t.Fatalf("%s did not settle: jobs=%s outbox=%s background=%s calls=%v", description, jobs, outbox, background, provider.Counts())
			}
			tasks := 1
			if automatic {
				tasks = 2
			}
			var first harness.Session
			for i := 0; i < tasks; i++ {
				session, err := c.NewSession(ctx, workspace)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = session
				}
				if automatic {
					shared := true
					if _, err = c.SetSkillLearningControl(ctx, session.ID, harness.SkillLearningControlRequest{Contribute: true, WorkspaceShared: &shared}); err != nil {
						t.Fatal(err)
					}
				}
				result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: fmt.Sprintf("Inspect complete JSON document %d through the delegated worker", i+1)}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
					return harness.AllowOnce, nil
				})
				if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
					t.Fatal("ordinary delegated input failed", result, err)
				}
				wait("actual native background completion", func() bool {
					var count int
					if err := c.store.DB().QueryRow(`SELECT count(*) FROM eino_background_tasks t JOIN harness_background_bindings b ON b.task_id=t.id WHERE b.parent_session_id=? AND t.status='completed'`, session.ID).Scan(&count); err != nil {
						t.Fatal(err)
					}
					return count == 1
				})
				wait("automatic delegated consolidation", func() bool {
					var count int
					if err := c.store.DB().QueryRow(`SELECT count(*) FROM df_learning_consolidations con JOIN df_learning_experiences exp ON exp.id=con.experience_id JOIN df_skill_maintenance_jobs j ON j.id=con.job_id WHERE exp.session_id=? AND j.state='completed'`, session.ID).Scan(&count); err != nil {
						t.Fatal(err)
					}
					return count == 1
				})
			}
			var discovery harness.SkillDiscovery
			var proposal harness.SkillProposal
			wait("system-generated delegated proposal", func() bool {
				page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Discoveries) == 0 {
					return false
				}
				if len(page.Discoveries) != 1 {
					t.Fatal(page)
				}
				discovery = page.Discoveries[0]
				if discovery.ProposalID == "" || discovery.State != "completed" {
					return false
				}
				proposal, err = c.GetSkillProposal(ctx, discovery.ProposalID)
				if err != nil {
					t.Fatal(err)
				}
				return proposal.Status == "published" && automatic || proposal.Status == "pending_review" && !automatic
			})
			if discovery.ReviewDraft != !automatic || len(discovery.Evidence) != tasks || proposal.Base != base || proposal.Evaluation.Behavior != "passed" ||
				proposal.Evaluation.BehaviorReport == nil || proposal.Evaluation.BehaviorReport.Improvements != 1 || proposal.Evaluation.BehaviorReport.Regressions != 0 {
				t.Fatal("native delegated evidence changed publication boundaries", discovery, proposal)
			}
			var inputs, roots, feedback int
			if err = c.store.DB().QueryRow(`SELECT count(*),count(DISTINCT root_id) FROM harness_learning_inputs`).Scan(&inputs, &roots); err != nil || inputs != tasks || roots != tasks {
				t.Fatal("actual child input or notification became independent learning authority", inputs, roots, err)
			}
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
				t.Fatal("native delegated stages fabricated quality feedback", feedback, err)
			}
			wire := newMissingManagement(t, ctx, c, nil)
			defer wire.host.Close()
			var history harness.SkillExperiencePage
			wire.wire(map[string]any{"operation": "skill.learning.experiences", "session_id": first.ID, "limit": 10}, "", &history)
			if len(history.Experiences) != 1 {
				t.Fatal(history)
			}
			x := history.Experiences[0]
			source, err := c.SkillLearningSource(ctx, first.ID, harness.SkillLearningSourceRequest{Kind: "experience", ID: x.ID, ExpectedRevision: x.Revision, Scope: "session", ExperienceID: x.ID})
			if err != nil || len(source.References) != 2 || source.SourceSessionID != first.ID {
				t.Fatal(source, err)
			}
			encoded, _ := json.Marshal(source)
			if strings.Contains(string(encoded), "NATIVE_PRIVATE") || strings.Contains(string(encoded), "DELEGATED_ONLY_NATIVE_INPUT") {
				t.Fatal("native source inspection leaked private child instructions")
			}
			if !automatic {
				head, err := c.GetSkillHead(ctx, "project", "research")
				if err != nil || head != base {
					t.Fatal("delegated review draft changed the live skill before approval", head, err)
				}
				var approved struct {
					Proposal harness.SkillProposal `json:"proposal"`
				}
				wire.wire(map[string]any{"operation": "proposal.approve", "proposal_id": proposal.ID, "expected_state_version": proposal.StateVersion, "expected_base_sha256": base.Version}, "", &approved)
				proposal = approved.Proposal
				if proposal.Status != "published" {
					t.Fatal(proposal)
				}
			} else {
				page, err := c.ListSkillAutoPatchObservations(ctx, harness.SkillQuery{Limit: 10})
				if err != nil || len(page.Observations) != 1 || page.Observations[0].Status != "observing" || page.Observations[0].LearningSource == nil ||
					page.Observations[0].LearningSource.DiscoveryID != discovery.ID || page.Observations[0].Successes != 0 || page.Observations[0].Failures != 0 {
					t.Fatal("delegated publication bypassed observation or manufactured effectiveness votes", page, err)
				}
			}
			counts := provider.Counts()
			if counts["foreground"] != 2*tasks || counts["child"] != 3*tasks || counts["extract"] != tasks || counts["consolidate"] != tasks || counts["match"] != tasks || counts["generate"] != 1 || counts["security"] != 1 || counts["quality"] != 1 || counts["behavior"] != 4 {
				t.Fatal("native learning replayed delegated tools or reviews", counts)
			}
			var receipts, tokens, unsettled int
			if err = c.store.DB().QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls`).Scan(&receipts, &tokens, &unsettled); err != nil ||
				receipts != 3*tasks+7 || tokens != receipts*125 || unsettled != 0 {
				t.Fatal("native delegated stages lost shared settled accounting", receipts, tokens, unsettled, err)
			}
			if err = wire.host.Close(); err != nil {
				t.Fatal(err)
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.SkillLearning, cfg.SkillGeneration, cfg.SkillDiscovery, cfg.SkillAutoPatch, cfg.SkillBehavior = nil, nil, nil, nil, nil
			cfg.SkillBehaviorEvaluator, cfg.SkillValidator, cfg.Skills.Install = nil, nil, nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			head, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || head.Version != proposal.CandidateVersion {
				t.Fatal("native delegated publication did not survive restart", head, err)
			}
			if _, err = c.LoadSession(ctx, first.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			restoredSource, err := c.SkillLearningSource(ctx, first.ID, source.Source)
			restoredJSON, _ := json.Marshal(restoredSource)
			if err != nil || string(restoredJSON) != string(encoded) {
				t.Fatal("disabled native restart lost redacted delegated source inspection", restoredSource, err)
			}
			if after := provider.Counts(); !maps.Equal(after, counts) {
				t.Fatal("disabled restart made a maintenance or delegated model call", after, counts)
			}
			result, err := c.Run(ctx, first.ID, []harness.Content{{Type: "text", Text: "Use the adopted research skill through the delegated worker"}}, nil, nil)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatal("native adopted delegated run failed", result, err)
			}
			wait("published candidate read by the native child", func() bool {
				var completed int
				var version string
				if err := c.store.DB().QueryRow(`SELECT count(*),COALESCE(MAX(r.version),'') FROM eino_background_tasks t
 JOIN harness_background_bindings b ON b.task_id=t.id JOIN harness_skill_reads r ON r.run_id=t.id
 WHERE b.origin_run_id=? AND t.status='completed' AND r.name='research' AND r.path='SKILL.md'`, result.Execution.RunID).Scan(&completed, &version); err != nil {
					t.Fatal(err)
				}
				return completed == 1 && version == head.Version
			})
			counts["foreground"] += 2
			counts["child"] += 3
			if after := provider.Counts(); !maps.Equal(after, counts) {
				t.Fatal("disabled learning replayed maintenance during actual published-skill use", after, counts)
			}
		})
	}
}
