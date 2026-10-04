package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManageNativeApprovalAcrossRestart(t *testing.T) {
	for _, placement := range []string{"root", "foreground", "background"} {
		for _, decision := range []string{"allow", "deny", "source-revoked", "builtin-allow", "builtin-policy-removed", "builtin-policy-added"} {
			t.Run(placement+"/"+decision, func(t *testing.T) {
				workspace, cfg := skillFixture(t)
				cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
				cfg.SkillProposalSources = []string{"project"}
				if decision == "builtin-allow" || decision == "builtin-policy-removed" {
					cfg.Skills.BuiltinSources = []string{"project"}
				}
				policyChanged := decision == "source-revoked" || decision == "builtin-policy-removed" || decision == "builtin-policy-added"
				cfg.PermissionMode = harness.PermissionModeDangerous
				cfg.BackgroundWorkers = 1
				var reviews atomic.Int32
				cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
					reviews.Add(1)
					return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
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
						http.Error(w, "fixture", 400)
						return
					}
					mu.Lock()
					calls[request.Model]++
					number := calls[request.Model]
					mu.Unlock()
					if number == 1 {
						if request.Model == "parent" && placement != "root" {
							if placement == "background" {
								skillToolResponse(w, request.Stream, "background_agent", `{"role":"editor","instruction":"Propose a skill improvement"}`, "")
							} else {
								skillToolResponse(w, request.Stream, "task", `{"subagent_type":"editor","prompt":"Propose a skill improvement","description":"Improve research"}`, "")
							}
							return
						}
						skillToolResponse(w, request.Stream, "skill_manage", `{"action":"patch","name":"research","reason":"Clarify the reusable steps","find":"BODY_ONLY_AFTER_LOAD","replace":"IMPROVED_STEPS"}`, "")
						return
					}
					skillToolResponse(w, request.Stream, "", "", "Proposal processed")
				}))
				defer server.Close()
				cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
				if placement != "root" {
					cfg.DisableSubagents = false
					cfg.ModelRoutes = map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", BaseURL: server.URL + "/v1", APIKey: "fixture"}}
					cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"editor": {Description: "Improve skills", SystemPrompt: "Propose changes", Model: "child", Tools: []string{"skill_manage"}, Skills: []string{"research"}}}}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
				s, err := c.NewSession(ctx, workspace)
				if err != nil {
					t.Fatal(err)
				}
				before, err := c.GetSkillHead(ctx, "project", "research")
				if err != nil {
					t.Fatal(err)
				}
				result, err := c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Improve research"}}, nil, func(_ context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
					if req.ToolName == "background_agent" {
						return harness.AllowOnce, nil
					}
					if req.ToolName != "skill_manage" {
						t.Errorf("unexpected approval: %s", req.ToolName)
					}
					return harness.PermissionCancelled, nil
				})
				if err != nil || result.Execution == nil {
					t.Fatalf("initial: %+v %v", result, err)
				}
				var task harness.BackgroundTask
				waitTask := func() {
					for task.Status == "pending" || task.Status == "running" {
						task, err = c.WaitBackgroundTask(ctx, s.ID, task.ID, task.Version)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if placement == "background" {
					tasks, err := c.BackgroundTasks(ctx, s.ID, "", 10)
					if err != nil || len(tasks) != 1 {
						t.Fatalf("tasks: %+v %v", tasks, err)
					}
					task = tasks[0]
					waitTask()
					if task.Status != "waiting_input" || task.Interaction == nil {
						t.Fatalf("not paused: %+v", task)
					}
				} else if result.Execution.Status != harness.ExecutionWaitingInput {
					t.Fatalf("not paused: %+v", result.Execution)
				}
				assertProposals := func(want int) {
					page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
					if err != nil || len(page.Proposals) != want || reviews.Load() != int32(want) {
						t.Fatalf("candidate count %d/%d reviews=%d: %v", len(page.Proposals), want, reviews.Load(), err)
					}
					if want == 1 && page.Proposals[0].Status != "pending_review" {
						t.Fatalf("candidate: %+v", page.Proposals[0])
					}
				}
				assertProposals(0)
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				c = nil
				if decision == "source-revoked" {
					cfg.SkillProposalSources = nil
				}
				if decision == "builtin-policy-removed" {
					cfg.Skills.BuiltinSources = nil
				}
				if decision == "builtin-policy-added" {
					cfg.Skills.BuiltinSources = []string{"project"}
				}
				c, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.LoadSession(ctx, s.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				assertProposals(0)
				choice := harness.AllowOnce
				if decision == "deny" {
					choice = harness.RejectOnce
				}
				if placement == "background" {
					task, err = c.BackgroundTask(ctx, s.ID, task.ID)
					if err != nil || task.Interaction == nil {
						t.Fatalf("reopened: %+v %v", task, err)
					}
					approval := harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: choice}
					task, err = c.ApproveBackgroundTask(ctx, s.ID, task.ID, approval)
					if err == nil {
						waitTask()
					}
					if !policyChanged && (err != nil || task.Status != "completed") {
						t.Fatalf("approved: %+v %v", task, err)
					}
					if policyChanged && err == nil && task.Status == "completed" {
						t.Fatal("changed candidate policy resumed")
					}
				} else {
					resume := harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}
					continued, resumeErr := c.ResumeExecution(ctx, s.ID, resume, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
						return choice, nil
					})
					if policyChanged {
						if resumeErr == nil {
							t.Fatal("changed candidate policy resumed")
						}
					} else {
						if resumeErr != nil || continued.Execution == nil || continued.Execution.Status != harness.ExecutionCompleted {
							t.Fatalf("resume: %+v %v", continued, resumeErr)
						}
						if _, err := c.ResumeExecution(ctx, s.ID, resume, nil, nil); !errors.Is(err, harness.ErrExecutionConflict) {
							t.Fatalf("stale approval replay: %v", err)
						}
					}
				}
				want := 0
				if decision == "allow" || decision == "builtin-allow" {
					want = 1
				}
				assertProposals(want)
				if after, err := c.GetSkillHead(ctx, "project", "research"); err != nil || after != before {
					t.Fatalf("approval published a skill: %+v %v", after, err)
				}
				var uncertain int
				if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE state IN ('started','uncertain')`).Scan(&uncertain); err != nil || uncertain != 0 {
					t.Fatalf("uncertain candidate effect: %d %v", uncertain, err)
				}
			})
		}
	}
}
