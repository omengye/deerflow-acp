package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManageGetWithoutIDFailsWithRecoveryGuidance(t *testing.T) {
	workspace, cfg := skillFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			skillToolResponse(w, request.Stream, "skill_manage", `{"action":"get"}`, "")
			return
		}
		found := false
		for _, m := range request.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "get requires the proposal_id") && strings.Contains(m.Content, "never write registry tables") {
				found = true
			}
		}
		if !found {
			t.Error("missing actionable get failure")
		}
		skillToolResponse(w, request.Stream, "", "", "Publication has not occurred.")
	}))
	defer server.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
	cfg.SkillProposalSources = []string{"project"}
	cfg.PermissionMode = harness.PermissionModeOff
	cfg.SkillValidator = skillValidatorFunc(func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error) {
		t.Error("invalid get reached review")
		return harness.SkillValidationReport{}, nil
	})
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(context.Background(), x.ID, []harness.Content{{Type: "text", Text: "Publish my new skill"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListSkillProposals(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Proposals) != 0 || calls.Load() != 2 {
		t.Fatal(page, calls.Load(), err)
	}
}
