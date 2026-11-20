package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManageAutomaticSubmissionSurvivesRestartWithoutApproval(t *testing.T) {
	for _, placement := range []string{"root", "foreground", "background"} {
		t.Run(placement, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeAll
			cfg.BackgroundWorkers = 1
			cfg.SkillValidator = skillValidatorFunc(func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				t.Error("submission ran synchronous validation")
				return harness.SkillValidationReport{}, harness.ErrPermissionDenied
			})
			var mu sync.Mutex
			calls := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model  string
					Stream bool
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				calls[request.Model]++
				number := calls[request.Model]
				mu.Unlock()
				if number == 1 {
					if placement != "root" && request.Model == "parent" {
						if placement == "background" {
							skillToolResponse(w, request.Stream, "background_agent", `{"role":"editor","instruction":"Propose a skill improvement"}`, "")
						} else {
							skillToolResponse(w, request.Stream, "task", `{"subagent_type":"editor","prompt":"Propose improvement","description":"Improve research"}`, "")
						}
						return
					}
					skillToolResponse(w, request.Stream, "skill_manage", `{"action":"patch","name":"research","reason":"Clarify steps","find":"BODY_ONLY_AFTER_LOAD","replace":"IMPROVED_STEPS"}`, "")
					return
				}
				skillToolResponse(w, request.Stream, "", "", "Candidate queued")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
			if placement != "root" {
				cfg.DisableSubagents = false
				cfg.ModelRoutes = map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", BaseURL: server.URL + "/v1", APIKey: "fixture"}}
				cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"editor": {Description: "Improve skills", SystemPrompt: "Propose changes", Model: "child", Tools: []string{"skill_manage"}, Skills: []string{"research"}}}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			configureSkillManageQueue(t, c)
			s, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			before, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Improve research"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
				if p.ToolName == "skill_manage" {
					t.Error("automatic skill candidate requested user approval")
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if err != nil || result.StopReason != "end_turn" {
				t.Fatal(result, err)
			}
			if placement == "background" {
				tasks, e := c.BackgroundTasks(ctx, s.ID, "", 10)
				if e != nil || len(tasks) != 1 {
					t.Fatal(tasks, e)
				}
				task := tasks[0]
				for task.Status == "pending" || task.Status == "running" {
					task, e = c.WaitBackgroundTask(ctx, s.ID, task.ID, task.Version)
					if e != nil {
						t.Fatal(e)
					}
				}
				if task.Status != "completed" {
					t.Fatal(task)
				}
			}
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
			if err != nil || len(page.Proposals) != 1 || page.Proposals[0].Origin != "automatic" || page.Proposals[0].GenerationJobID == "" || page.Proposals[0].BehaviorJobID != "" {
				t.Fatal(page, err)
			}
			saved := page.Proposals[0]
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			reviews := 0
			cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				reviews++
				return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed"}, nil
			})
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.GetSkillProposal(ctx, saved.ID)
			if err != nil || got.GenerationJobID != saved.GenerationJobID || got.BehaviorJobID != "" || got.CandidateVersion != saved.CandidateVersion || got.Status != "validating" {
				t.Fatal(got, err)
			}
			after, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || after != before {
				t.Fatal("queued candidate changed live skill", after, err)
			}
			configureSkillManageQueue(t, c)
			if worked, err := c.evolution.RunGeneration(ctx); !worked || err != nil {
				t.Fatal(worked, err)
			}
			got, err = c.GetSkillProposal(ctx, saved.ID)
			if err != nil || got.Status != "published" || got.Approval == nil || got.Approval.SecurityReview == nil || got.CandidateVersion != saved.CandidateVersion || reviews != 1 {
				t.Fatal("worker did not publish the saved package after restart", got, reviews, err)
			}
		})
	}
}
