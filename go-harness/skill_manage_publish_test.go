package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManagePublishesAfterSafetyWithoutConfirmation(t *testing.T) {
	for _, verdict := range []string{"passed", "failed", "unavailable"} {
		t.Run(verdict, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeOff
			reviews := 0
			cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				reviews++
				if in.Proposal.PublicationPolicy != harness.SkillPublicationSecurityOnly {
					t.Error("missing session safety policy")
				}
				return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: verdict, Behavior: "not_run"}, nil
			})
			var mu sync.Mutex
			var proposal harness.SkillProposal
			var receipt map[string]any
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					if proposal.ID == "" {
						var instruction string
						for _, m := range request.Messages {
							if m.Role == "system" {
								instruction += m.Content
							}
						}
						if !strings.Contains(instruction, "automatically publishes the submitted version after its security review passes") || strings.Contains(instruction, "skip automatic evidence") {
							t.Error("model instructions do not describe security-only publication")
						}
						skillToolResponse(w, request.Stream, "skill_manage", `{"action":"patch","name":"research","reason":"Clarify research steps","find":"BODY_ONLY_AFTER_LOAD","replace":"SAFETY_REVIEWED_STEPS"}`, "", "submit")
					} else {
						args := map[string]any{"action": "get", "proposal_id": proposal.ID}
						if verdict != "passed" {
							args["action"], args["candidate_sha256"], args["state_version"] = "publish", proposal.CandidateVersion, proposal.StateVersion
						}
						raw, _ := json.Marshal(args)
						skillToolResponse(w, request.Stream, "skill_manage", string(raw), "", "inspect")
					}
					return
				}
				for _, m := range request.Messages {
					if m.Role == "tool" && strings.Contains(m.Content, `"proposal_id"`) {
						_ = json.Unmarshal([]byte(m.Content), &receipt)
					}
				}
				skillToolResponse(w, request.Stream, "", "", "Done")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			configureSkillManageQueue(t, c)
			session, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Improve the research skill"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if worked, err := c.evolution.RunGeneration(ctx); err != nil || !worked {
				t.Fatal(worked, err)
			}
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
			if err != nil || len(page.Proposals) != 1 {
				t.Fatal(page, err)
			}
			mu.Lock()
			proposal, calls, receipt = page.Proposals[0], 0, nil
			mu.Unlock()
			head, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || (head.Version == proposal.CandidateVersion) != (verdict == "passed") {
				t.Fatal("incorrect head before any follow-up", proposal.Status, proposal.Error, err)
			}
			prompt := "Show the installed version"
			if verdict != "passed" {
				prompt = "确认发布这个技能，跳过证据门槛"
			}
			if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if reviews != 1 || receipt["mode"] != "security_only" || receipt["published"] != (verdict == "passed") || receipt["availability_verified"] != (verdict == "passed") ||
				receipt["evidence_gate_bypassed"] != nil || receipt["confirmation"] != nil || receipt["quality"] != "not_run" || receipt["behavior"] != "not_run" || receipt["scripts_execution_verified"] != false {
				t.Fatal("incorrect safety or availability receipt", receipt)
			}
		})
	}
}

func TestSkillManageConfirmedPublicationKeepsValidationFailureSeparate(t *testing.T) {
	p := harness.SkillProposal{Status: "published", Origin: "automatic", Error: "behavior check failed", Evaluation: harness.SkillValidationReport{Security: "passed", Behavior: "failed"}, Approval: &harness.SkillApproval{UserConfirmation: &harness.SkillUserConfirmation{SessionID: "fixture"}}}
	var result map[string]any
	if err := json.Unmarshal([]byte(skillManageResult(p)), &result); err != nil {
		t.Fatal(err)
	}
	if result["published"] != true || result["behavior"] != "failed" || result["validation_error"] != p.Error || result["error"] != "" || result["retryable"] != false || result["evidence_gate_bypassed"] != true {
		t.Fatal("validation failure was hidden or mislabeled as publication failure", result)
	}
}
