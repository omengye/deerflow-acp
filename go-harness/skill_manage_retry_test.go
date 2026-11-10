package deerflow

import (
	"context"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSkillManageRetiredActionsCannotMutateCandidates(t *testing.T) {
	for _, action := range []string{"review_retry", "approve", "delete"} {
		t.Run(action, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.SkillProposalSources = []string{"project"}
			cfg.SkillValidator = skillValidatorFunc(func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error) {
				t.Error("retired action reached validator")
				return harness.SkillValidationReport{}, nil
			})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct{ Role, Content string }
					Tools    []struct {
						Function struct {
							Name, Description string
							Parameters        json.RawMessage
						}
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if calls.Add(1) == 1 {
					for _, tool := range req.Tools {
						if tool.Function.Name == "skill_manage" {
							if strings.Contains(string(tool.Function.Parameters), `"review_retry"`) || !strings.Contains(string(tool.Function.Parameters), `"publish"`) || !strings.Contains(tool.Function.Description, "native user input") {
								t.Error("stale tool contract", tool.Function)
							}
						}
					}
					args, _ := json.Marshal(map[string]any{"action": action, "proposal_id": "p_private"})
					skillToolResponse(w, req.Stream, "skill_manage", string(args), "")
					return
				}
				found := false
				for _, m := range req.Messages {
					if m.Role == "tool" && strings.Contains(m.Content, `"code":"invalid_request"`) {
						found = true
					}
				}
				if !found {
					t.Error("retired action did not return a bounded rejection")
				}
				skillToolResponse(w, req.Stream, "", "", "Automatic pipeline only")
			}))
			defer server.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
			c, err := Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s, err := c.NewSession(context.Background(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(context.Background(), s.ID, []harness.Content{{Type: "text", Text: "Inspect candidate"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			page, err := c.ListSkillProposals(context.Background(), harness.SkillQuery{Limit: 10})
			if err != nil || len(page.Proposals) != 0 {
				t.Fatal(page, err)
			}
		})
	}
}
