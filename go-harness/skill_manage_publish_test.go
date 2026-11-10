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

func TestSkillManageNativeUserConfirmedPublication(t *testing.T) {
	for _, prompt := range []string{"我确认发布这个技能，跳过证据门槛。", "请继续修改这个技能。", "不要直接发布。"} {
		t.Run(prompt, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeOff
			cfg.SkillValidator = skillValidatorFunc(func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				t.Error("explicit publication must not fabricate a validation call")
				return harness.SkillValidationReport{}, harness.ErrPermissionDenied
			})
			var mu sync.Mutex
			phase, calls := 0, 0
			var proposal harness.SkillProposal
			var final map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream   bool                             `json:"stream"`
					Messages []struct{ Role, Content string } `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				if phase == 0 && calls == 1 {
					skillToolResponse(w, request.Stream, "skill_manage", `{"action":"patch","name":"research","reason":"Clarify research steps","find":"BODY_ONLY_AFTER_LOAD","replace":"CONFIRMED_RESEARCH_STEPS"}`, "", "submit")
					return
				}
				if phase == 1 {
					if calls == 1 {
						args, _ := json.Marshal(map[string]any{"action": "get", "proposal_id": proposal.ID})
						skillToolResponse(w, request.Stream, "skill_manage", string(args), "", "inspect")
						return
					}
					if calls == 2 {
						args, _ := json.Marshal(map[string]any{"action": "publish", "proposal_id": proposal.ID, "candidate_sha256": proposal.CandidateVersion, "state_version": proposal.StateVersion})
						skillToolResponse(w, request.Stream, "skill_manage", string(args), "", "publish")
						return
					}
					for _, m := range request.Messages {
						if m.Role == "tool" && strings.Contains(m.Content, `"code":"user_confirmation_required"`) {
							final = map[string]any{"code": "user_confirmation_required"}
						} else if m.Role == "tool" && strings.Contains(m.Content, `"evidence_gate_bypassed"`) {
							if err := json.Unmarshal([]byte(m.Content), &final); err != nil {
								t.Error(err)
							}
						}
					}
				}
				skillToolResponse(w, request.Stream, "", "", "Operation finished")
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
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
			if err != nil || len(page.Proposals) != 1 {
				t.Fatal("candidate missing", err)
			}
			mu.Lock()
			proposal, phase, calls = page.Proposals[0], 1, 0
			mu.Unlock()
			if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			head, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(prompt, "我确认") {
				if head.Version != proposal.CandidateVersion || final["published"] != true || final["mode"] != "user_confirmed" || final["availability_verified"] != true || final["evidence_gate_bypassed"] != true || final["live_skill_changed"] != true {
					t.Fatalf("publication receipts missing: %+v", final)
				}
				if final["scripts_execution_verified"] == true {
					t.Fatal("unrun scripts reported as executed")
				}
			} else if head != proposal.Base || final["code"] != "user_confirmation_required" {
				t.Fatalf("nonconfirmation mutated live head: %+v", final)
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
