package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestPublicationModelResultDescribesSavedDecision(t *testing.T) {
	p := harness.SkillProposal{ID: "p_one", Status: "pending_review", Approval: &harness.SkillApproval{}}
	var result map[string]any
	if err := json.Unmarshal([]byte(skillManageResult(p)), &result); err != nil {
		t.Fatal(err)
	}
	if result["requires_operator_review"] != nil || result["approval_recorded"] != nil || result["published"] != false {
		t.Fatal("approval confused with publication", result)
	}
	p.Status = "published"
	_ = json.Unmarshal([]byte(skillManageResult(p)), &result)
	if result["published"] != true || result["live_skill_changed"] != false {
		t.Fatal("get confused publication state with its own side effects", result)
	}
}

func TestPublicationHostRestartResumesSavedApproval(t *testing.T) {
	c, cfg := skillEvolutionClient(t)
	ctx := context.Background()
	p := submitSkillEdit(t, c, "restart-approved", "Approved publication after restart")
	if _, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.publish_retry", "proposal_id": p.ID, "expected_approval_version": p.StateVersion}); err == nil {
		t.Fatal("retry approved an unapproved candidate")
	}
	db := c.skills.PublicationStore()
	if _, err := db.Exec(`CREATE TRIGGER fail_publish BEFORE INSERT ON df_skill_publications BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	approved, err := c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "desktop"})
	if err == nil || approved.Approval == nil || approved.Status != "pending_review" {
		t.Fatal("fixture did not interrupt publication after approval", err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_publish`); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SkillValidator = skillValidatorFunc(func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error) {
		t.Error("restart must not rerun paid review")
		return harness.SkillValidationReport{}, errors.New("unexpected review")
	})
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.evolution.Get(ctx, p.ID)
	if err != nil || got.Status != "published" || got.Approval == nil || *got.Approval != *approved.Approval {
		t.Fatal("startup did not resume the exact decision", err)
	}
}

func TestPublicationManagementCannotApproveOrReviveHistoricalCandidates(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	ctx := context.Background()
	p := submitSkillEdit(t, c, "historical", "Archived improvement")
	for _, operation := range []string{"proposal.approve", "proposal.submit", "proposal.source_review", "proposal.review_retry", "proposal.publish_retry"} {
		_, err := skillManagementCall(t, c, map[string]any{"operation": operation, "proposal_id": p.ID, "expected_state_version": p.StateVersion})
		if err == nil {
			t.Fatal("manual candidate recovery remains reachable", operation)
		}
	}
	after, err := c.GetSkillProposal(ctx, p.ID)
	if err != nil || after.StateVersion != p.StateVersion || after.Approval != nil {
		t.Fatal("retired operation mutated historical evidence", after, err)
	}
}

func TestPublishedSkillAvailableNextTurnInSameSession(t *testing.T) {
	workspace, cfg := skillFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct{ Stream bool }
		_ = json.Unmarshal(data, &request)
		switch calls.Add(1) {
		case 1:
			if strings.Contains(string(data), "published-search") {
				t.Error("unpublished skill visible")
			}
			skillToolResponse(w, request.Stream, "", "", "First turn complete")
		case 2:
			if !strings.Contains(string(data), "published-search") {
				t.Error("same session missed newly published metadata")
			}
			skillToolResponse(w, request.Stream, "skill", `{"skill":"published-search"}`, "")
		case 3:
			if !strings.Contains(string(data), "NEW_PUBLISHED_BODY") {
				t.Error("published skill could not load in same session")
			}
			skillToolResponse(w, request.Stream, "", "", "Published skill loaded")
		default:
			t.Error("unexpected model request")
			skillToolResponse(w, request.Stream, "", "", "done")
		}
	}))
	defer server.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
	cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, input harness.SkillValidationInput) (harness.SkillValidationReport, error) {
		return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
	})
	ctx := context.Background()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "First turn"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	p, err := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "published-search", Action: "create", RequestKey: "same-session", Reason: "Reusable search", Files: map[string][]byte{"SKILL.md": []byte("---\nname: published-search\ndescription: Search published sources\n---\nNEW_PUBLISHED_BODY\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Use the new skill"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("skill load not exercised", calls.Load())
	}
}
