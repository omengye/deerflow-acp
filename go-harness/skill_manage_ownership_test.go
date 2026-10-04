package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManageNativeProposalOwnership(t *testing.T) {
	for _, author := range []string{"session", "role"} {
		t.Run(author, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.SkillProposalSources = []string{"project"}
			cfg.PermissionMode = harness.PermissionModeOff
			var reviews atomic.Int32
			cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				reviews.Add(1)
				return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
			})
			var mu sync.Mutex
			phase, proposalID, observed := "submit", "", ""
			calls := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "fixture", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				key := phase + "/" + req.Model
				calls[key]++
				if calls[key] == 1 {
					if phase == "submit" {
						if author == "role" && req.Model == "parent" {
							skillToolResponse(w, req.Stream, "task", `{"subagent_type":"editor","prompt":"Propose improvement","description":"Improve research"}`, "")
						} else {
							skillToolResponse(w, req.Stream, "skill_manage", `{"action":"patch","name":"research","reason":"Make the guidance clearer","find":"BODY_ONLY_AFTER_LOAD","replace":"NEW_GUIDANCE"}`, "")
						}
					} else {
						skillToolResponse(w, req.Stream, "skill_manage", fmt.Sprintf(`{"action":"get","proposal_id":%q}`, proposalID), "")
					}
					return
				}
				for _, msg := range req.Messages {
					if msg.Role == "tool" {
						observed = msg.Content
					}
				}
				skillToolResponse(w, req.Stream, "", "", "Finished")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "parent", server.URL+"/v1", "fixture"
			if author == "role" {
				cfg.DisableSubagents = false
				cfg.ModelRoutes = map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", BaseURL: server.URL + "/v1", APIKey: "fixture"}}
				cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"editor": {Description: "Improve skills", SystemPrompt: "Propose improvements", Model: "child", Tools: []string{"skill_manage"}, Skills: []string{"research"}}}}
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
			if _, err = c.Run(ctx, s.ID, []harness.Content{{Type: "text", Text: "Propose improvement"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
			if err != nil || len(page.Proposals) != 1 {
				t.Fatalf("proposals: %+v %v", page, err)
			}
			mu.Lock()
			proposalID = page.Proposals[0].ID
			mu.Unlock()
			// Read authority survives reopening the same session. A root cannot
			// inherit its child's authorship, and another session cannot borrow it.
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.LoadSession(ctx, s.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			for _, reader := range []string{"same-session", "other-session"} {
				sessionID := s.ID
				if reader == "other-session" {
					other, err := c.NewSession(ctx, workspace)
					if err != nil {
						t.Fatal(err)
					}
					sessionID = other.ID
				}
				mu.Lock()
				phase, observed = reader, ""
				mu.Unlock()
				if _, err = c.Run(ctx, sessionID, []harness.Content{{Type: "text", Text: "Get the proposal status"}}, nil, nil); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				output := observed
				mu.Unlock()
				allowed := author == "session" && reader == "same-session"
				if allowed {
					if !strings.Contains(output, `"status":"pending_review"`) || !strings.Contains(output, proposalID) {
						t.Fatalf("owner lost access: %s", output)
					}
				} else if !strings.Contains(output, `"code":"not_found"`) || strings.Contains(output, proposalID) {
					t.Fatalf("another agent's proposal exposed to %s: %s", reader, output)
				}
			}
			if reviews.Load() != 1 {
				t.Fatalf("reading repeated model review: %d", reviews.Load())
			}
		})
	}
}
