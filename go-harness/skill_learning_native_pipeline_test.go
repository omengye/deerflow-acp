package deerflow

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skilllearningtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const nativeLearningInstruction = skilllearningtest.Instruction

func TestSkillLearningNativePipelineAutomaticReviewAndPublication(t *testing.T) {
	nativeLearningPipeline(t, false)
}

func TestSkillLearningNativeFollowupResolutionAutomaticReviewAndPublication(t *testing.T) {
	nativeLearningPipeline(t, true)
}

func TestSkillLearningNativeAutomaticPipelineSurvivesHistoricalUnfinishedTasks(t *testing.T) {
	for _, status := range []string{"waiting_input", "needs_reconciliation"} {
		t.Run(status, func(t *testing.T) { nativeLearningPipeline(t, false, status) })
	}
}

func nativeLearningPipeline(t *testing.T, resolution bool, historicalStatus ...string) {
	t.Helper()
	for _, automatic := range []bool{false, true} {
		name := "review-draft"
		if automatic {
			name = "automatic-existing"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			workspace, cfg := skillFixture(t)
			if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"verified":true,"token":"NATIVE_PRIVATE_TOKEN"}`), 0600); err != nil {
				t.Fatal(err)
			}
			provider := skilllearningtest.New(t)
			if resolution {
				provider = skilllearningtest.FollowupResolution(t)
			}
			server := httptest.NewServer(provider)
			defer server.Close()
			route := func(model string) harness.ModelRoute {
				return harness.ModelRoute{Provider: "openai", Model: model, BaseURL: server.URL + "/v1", APIKey: "fixture-only"}
			}
			model := func(name string) harness.SkillGenerationModelConfig {
				return harness.SkillGenerationModelConfig{Route: route(name), Timeout: 10 * time.Second, MaxOutputTokens: 1024}
			}
			cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL = "openai", "foreground", "fixture-only", server.URL+"/v1"
			cfg.ToolPolicy.Allowlist = []string{"skill", "read_file"}
			cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 6, MaxToolCalls: 4, MaxTokens: 100000, MaxOutputTokens: 512}
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 20, MaxDailyTokens: 200000, MaxJobSeconds: 60, Workers: 1}
			consolidation, matching := model("consolidate"), model("match")
			cfg.SkillLearning = &harness.SkillLearningOptions{Model: model("extract"), Consolidation: &consolidation, Matching: &matching,
				CrossSession: automatic, Idle: time.Second, Budget: budget, MaxDailyModelCalls: 6, MaxDailyTokens: 100000}
			if resolution {
				cfg.SkillLearning.MaxDailyModelCalls = 10
			}
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
			cfg.SkillBehavior = &harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{{Version: "skill-behavior-v1", ID: "native-json-check", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{
				{ID: "targeted", Kind: "targeted", Prompt: "Inspect the complete JSON document and report completion.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-native-complete", Kind: "output_equals", Expected: "verified"}}},
				{ID: "regression", Kind: "regression", Prompt: "Retain the existing source research behavior.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-native-retained", Kind: "output_equals", Expected: "retained"}}},
			}}}}
			cfg.SkillDiscovery = &harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, ExperienceReviewDraftsEnabled: !automatic,
				RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
			if automatic {
				cfg.SkillAutoPatch = &harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: true, MaxChangedLines: 40, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
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
			// This isolated fixture starts with an already-adopted managed skill;
			// no authoring, generation or episode operation drives its evolution.
			if err = c.skills.WithPublication(ctx, "project", "research", func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err != nil {
					return err
				}
				_, err = tx.Publish(head.Version, head, true)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			base, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || !base.Managed {
				t.Fatal(base, err)
			}
			wait := func(description string, ready func() bool) {
				t.Helper()
				// Share the fixture's bounded deadline across asynchronous stages;
				// race instrumentation can consume most of a separate 20s window.
				started := time.Now()
				for ctx.Err() == nil {
					if ready() {
						t.Logf("%s settled in %s", description, time.Since(started))
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				var jobs, outbox string
				_ = c.store.DB().QueryRow(`SELECT COALESCE(group_concat(job_kind||':'||state||':'||error_code),'') FROM df_skill_maintenance_jobs`).Scan(&jobs)
				_ = c.store.DB().QueryRow(`SELECT COALESCE(group_concat(state||':'||reason),'') FROM harness_learning_outbox`).Scan(&outbox)
				t.Fatalf("%s did not settle; jobs=%s outbox=%s calls=%v context=%v", description, jobs, outbox, provider.Counts(), ctx.Err())
			}
			var first harness.Session
			var historicalRuns []string
			tasks := 1
			if automatic {
				tasks = 2
			}
			for task := 0; task < tasks; task++ {
				session, err := c.NewSession(ctx, workspace)
				if err != nil {
					t.Fatal(err)
				}
				if task == 0 {
					first = session
				}
				if automatic {
					shared := true
					if _, err = c.SetSkillLearningControl(ctx, session.ID, harness.SkillLearningControlRequest{Contribute: true, WorkspaceShared: &shared}); err != nil {
						t.Fatal(err)
					}
				}
				if len(historicalStatus) != 0 {
					// Reproduce an accepted old task with its own unsettled budget
					// tree. Only the historical run's persisted state is seeded;
					// all subsequent turns and learning stages run normally.
					id := runtime.NewID()
					request := harness.RunRequest{Session: session, RunID: id, RootBudgetID: id, InputID: runtime.NewID(), Input: []harness.Content{{Type: "text", Text: fmt.Sprintf("Independent historical unfinished task %d", task+1)}}}
					if err := c.service.Store.BeginRun(ctx, request); err != nil {
						t.Fatal(err)
					}
					if _, err := c.store.DB().ExecContext(ctx, `UPDATE harness_runs SET status=? WHERE id=?`, historicalStatus[0], id); err != nil {
						t.Fatal(err)
					}
					historicalRuns = append(historicalRuns, id)
				}
				for turn, prompt := range []string{fmt.Sprintf("Inspect JSON document %d using research", task+1), "Correct the previous inspection: read the complete document and verify required fields"} {
					if result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
						return harness.AllowOnce, nil
					}); err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
						t.Fatal("native foreground run failed", result, err)
					}
					if resolution && turn == 0 {
						wait("unverified native first-turn pattern", func() bool {
							var completed int
							if err := c.store.DB().QueryRow(`SELECT count(*) FROM df_learning_consolidations c JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE j.state='completed'`).Scan(&completed); err != nil {
								t.Fatal(err)
							}
							return completed == 2*task+1
						})
						var discoveries int
						if err := c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_discoveries`).Scan(&discoveries); err != nil || discoveries != 0 || provider.Counts()["generate"] != 0 {
							t.Fatal("unverified first turn generated a proposal", discoveries, provider.Counts(), err)
						}
					}
				}
				if automatic && task == 0 {
					wait("first native task matching", func() bool {
						var completed int
						if err := c.store.DB().QueryRow(`SELECT count(*) FROM df_learning_matches m JOIN df_skill_maintenance_jobs j ON j.id=m.job_id WHERE j.state='completed'`).Scan(&completed); err != nil {
							t.Fatal(err)
						}
						return completed == 1
					})
					if page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10}); err != nil || len(page.Discoveries) != 0 || provider.Counts()["generate"] != 0 {
						t.Fatal("one independent task unexpectedly authorized automatic publication", page, err)
					}
				}
			}
			var discovery harness.SkillDiscovery
			var proposal harness.SkillProposal
			wait("native automatic proposal", func() bool {
				page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Discoveries) == 0 {
					return false
				}
				if len(page.Discoveries) != 1 {
					t.Fatal("automatic pipeline created duplicate discoveries", page)
				}
				discovery = page.Discoveries[0]
				if discovery.State == "failed" || discovery.State == "cancelled" {
					t.Fatal("native generation failed", discovery)
				}
				if discovery.ProposalID == "" || discovery.State != "completed" {
					return false
				}
				proposal, err = c.GetSkillProposal(ctx, discovery.ProposalID)
				if err != nil {
					t.Fatal(err)
				}
				if automatic {
					return proposal.Status == "published"
				}
				return proposal.Status == "pending_review"
			})
			if discovery.ReviewDraft != !automatic || discovery.HostAdjudicated || discovery.CreationRetry != nil || len(discovery.Evidence) != tasks ||
				proposal.Origin != "automatic" || proposal.Author != "discovery" || proposal.Trigger.Type != "experience_pattern" || proposal.Base != base ||
				!slices.Equal(proposal.ChangedFiles, []string{"SKILL.md"}) || proposal.Evaluation.Security != "passed" || proposal.Evaluation.Behavior != "passed" ||
				proposal.Evaluation.BehaviorReport == nil || proposal.Evaluation.BehaviorReport.Improvements != 1 || proposal.Evaluation.BehaviorReport.Regressions != 0 {
				t.Fatal("native pipeline lost source, review or actual comparison boundaries", discovery, proposal)
			}
			counts := provider.Counts()
			for _, run := range historicalRuns {
				var status string
				var included int
				if err := c.store.DB().QueryRowContext(ctx, `SELECT status FROM harness_runs WHERE id=?`, run).Scan(&status); err != nil || status != historicalStatus[0] {
					t.Fatal("learning changed an unfinished business task", status, err)
				}
				if err := c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM df_learning_extractions WHERE instr(CAST(payload AS TEXT),?)>0`, run).Scan(&included); err != nil || included != 0 {
					t.Fatal("learning included an unsettled historical source", included, err)
				}
			}
			batches, foreground := tasks, 6*tasks
			if resolution {
				batches, foreground = 2*tasks, 4*tasks
				var members, observations, support, links int
				if err := c.store.DB().QueryRow(`SELECT json_array_length(CAST(r.payload AS TEXT),'$.members'),
 json_extract(CAST(r.payload AS TEXT),'$.pattern.observed_episodes'),json_extract(CAST(r.payload AS TEXT),'$.pattern.support_episodes'),
 json_array_length(CAST(r.payload AS TEXT),'$.resolutions') FROM df_learning_pattern_revisions r JOIN df_learning_patterns p ON p.id=r.pattern_id AND p.current_revision=r.revision`).Scan(&members, &observations, &support, &links); err != nil || members != 2*tasks || observations != tasks || support != tasks || links != tasks {
					t.Fatal("native followup dropped unresolved history or fabricated independent observations", members, observations, support, links, err)
				}
			}
			for _, stage := range []string{"extract", "consolidate", "match"} {
				want := batches
				if stage == "match" {
					want = tasks
				}
				if counts[stage] != want {
					t.Fatal("native stage retried or missed an episode", stage, counts)
				}
			}
			if counts["foreground"] != foreground || counts["generate"] != 1 || counts["security"] != 1 || counts["quality"] != 1 || counts["behavior"] != 4 {
				t.Fatal("native stages replayed foreground tools or bypassed fresh variant evaluations", counts)
			}
			var receipts, tokens, unsettled, feedback int
			if err = c.store.DB().QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls`).Scan(&receipts, &tokens, &unsettled); err != nil ||
				receipts != 2*batches+tasks+7 || tokens != receipts*125 || unsettled != 0 {
				t.Fatal("native maintenance stages did not share settled accounting", receipts, tokens, unsettled, err)
			}
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
				t.Fatal("model stages fabricated operator feedback", feedback, err)
			}
			wire := newMissingManagement(t, ctx, c, nil)
			defer wire.host.Close()
			var managedStatus harness.SkillLearningStatus
			wire.wire(map[string]any{"operation": "skill.learning.status"}, "", &managedStatus)
			if !managedStatus.Enabled || !managedStatus.ProposalDiscoveryEnabled || managedStatus.AutoPatchEnabled != automatic {
				t.Fatal("authenticated management lost the running evolution grants", managedStatus)
			}
			var managedDiscoveries harness.SkillDiscoveryPage
			wire.wire(map[string]any{"operation": "skill.discovery.list", "limit": 10}, "", &managedDiscoveries)
			if len(managedDiscoveries.Discoveries) != 1 || managedDiscoveries.Discoveries[0].ID != discovery.ID || managedDiscoveries.Discoveries[0].ReviewDraft != !automatic {
				t.Fatal("authenticated management lost automatic draft attribution", managedDiscoveries)
			}
			var managedProposal harness.SkillProposal
			wire.wire(map[string]any{"operation": "proposal.get", "proposal_id": proposal.ID}, "", &managedProposal)
			if managedProposal.CandidateVersion != proposal.CandidateVersion || managedProposal.Base != base || managedProposal.Evaluation.Behavior != "passed" ||
				!strings.Contains(managedProposal.Diff, "+"+nativeLearningInstruction) {
				t.Fatal("authenticated review omitted the generated diff or pinned evidence", managedProposal)
			}
			if !automatic {
				head, err := c.GetSkillHead(ctx, "project", "research")
				if err != nil || head != base {
					t.Fatal("review draft changed live skill before approval", head, err)
				}
				var approved struct {
					Proposal harness.SkillProposal `json:"proposal"`
				}
				wire.wire(map[string]any{"operation": "proposal.approve", "proposal_id": proposal.ID, "expected_state_version": proposal.StateVersion,
					"expected_base_sha256": base.Version}, "", &approved)
				proposal = approved.Proposal
				if proposal.Status != "published" {
					t.Fatal("system-generated draft could not be reviewed", proposal)
				}
			} else {
				page, err := c.ListSkillAutoPatchObservations(ctx, harness.SkillQuery{Limit: 10})
				if err != nil || len(page.Observations) != 1 || page.Observations[0].Status != "observing" || page.Observations[0].LearningSource == nil ||
					page.Observations[0].LearningSource.DiscoveryID != discovery.ID || page.Observations[0].Successes != 0 || page.Observations[0].Failures != 0 {
					t.Fatal("native automatic publication did not attach observation without quality votes", page, err)
				}
			}
			published, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || published.Version != proposal.CandidateVersion || !published.Managed || published == base {
				t.Fatal("native publication missing", published, err)
			}
			if err = wire.host.Close(); err != nil {
				t.Fatal(err)
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			// Turning learning off does not delete an adopted skill or restore its
			// old import. A new native run must really read the published version.
			cfg.SkillLearning, cfg.SkillGeneration, cfg.SkillDiscovery, cfg.SkillAutoPatch = nil, nil, nil, nil
			cfg.SkillBehavior, cfg.SkillBehaviorEvaluator = nil, nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.LoadSession(ctx, first.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(ctx, first.ID, []harness.Content{{Type: "text", Text: "Use the adopted research skill to inspect config.json"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatal("adopted skill failed in a new native run", result, err)
			}
			var readVersion string
			if err = c.store.DB().QueryRow(`SELECT version FROM harness_skill_reads WHERE run_id=? AND name='research' AND path='SKILL.md'`, result.Execution.RunID).Scan(&readVersion); err != nil || readVersion != published.Version {
				t.Fatal("new run loaded old skill instead of published candidate", readVersion, published, err)
			}
			for _, stage := range []string{"extract", "consolidate", "match", "generate", "security", "quality", "behavior"} {
				if provider.Counts()[stage] != counts[stage] {
					t.Fatal("review or reopen unexpectedly regenerated or evaluated a skill", stage, provider.Counts())
				}
			}
			original, err := os.ReadFile(filepath.Join(workspace, "skills", "research", "SKILL.md"))
			if err != nil || !strings.Contains(string(original), nativeLearningInstruction) {
				t.Fatal("evolution did not publish to authoritative source files", err)
			}
		})
	}
}
