package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestPublicationModelResultDescribesSavedDecision(t *testing.T) {
	p := harness.SkillProposal{ID: "p_one", Status: "pending_review", Approval: &harness.SkillApproval{}}
	var result map[string]any
	if err := json.Unmarshal([]byte(skillManageResult(p)), &result); err != nil {
		t.Fatal(err)
	}
	if result["requires_operator_review"] != false || result["approval_recorded"] != true || result["published"] != false {
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

func TestPublicationManagementRetainsApprovalOnFailureAndSupportsSourceReview(t *testing.T) {
	c, cfg := skillEvolutionClient(t)
	ctx := context.Background()
	content := []byte("---\nname: new-search\ndescription: Search sources\n---\nReview all results.\n")
	p, err := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "new-search", Action: "create", RequestKey: "candidate", Reason: "Reusable search", Files: map[string][]byte{"SKILL.md": content}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.Skills.Sources[0].Root, "new-search")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "SKILL.md"), append(content, []byte("Current directory revision.\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	host, err := localhost.New(localhost.Config{RuntimeDir: runtime, Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	command := map[string]any{"operation": "proposal.approve", "proposal_id": p.ID, "expected_state_version": p.StateVersion}
	request, _ := json.Marshal(command)
	response, err := localhost.ManagementCall(ctx, runtime, request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Proposal json.RawMessage `json:"proposal"`
		} `json:"data"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.OK || len(envelope.Data.Proposal) == 0 {
		t.Fatalf("lost definitive result: %s", response)
	}
	var saved harness.SkillProposal
	if json.Unmarshal(envelope.Data.Proposal, &saved) != nil || saved.Approval == nil || saved.Status != "stale" || saved.PublicationError != "target_content_differs" {
		t.Fatalf("lost approval: %s", response)
	}
	var projected map[string]any
	_ = json.Unmarshal(envelope.Data.Proposal, &projected)
	if projected["diff"] != "" || projected["publication"].(map[string]any)["state"] != "blocked" {
		t.Fatal("unsafe/incorrect error projection")
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "proposal.publish_retry", "proposal_id": p.ID, "expected_approval_version": p.StateVersion}); err == nil {
		t.Fatal("retry overwrote conflicting source")
	}
	result, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.source_review", "proposal_id": p.ID, "expected_state_version": saved.StateVersion, "request_key": "new-source"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	var fresh struct {
		Proposal harness.SkillProposal `json:"proposal"`
	}
	if json.Unmarshal(raw, &fresh) != nil || fresh.Proposal.ID == p.ID || fresh.Proposal.Approval != nil {
		t.Fatal("new source inherited approval")
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "proposal.approve", "proposal_id": fresh.Proposal.ID, "expected_state_version": fresh.Proposal.StateVersion}); err != nil {
		t.Fatal(err)
	}
	head, err := c.GetSkillHead(ctx, "project", "new-search")
	if err != nil || !head.Exists || !head.Enabled || head.Version != fresh.Proposal.CandidateVersion {
		t.Fatal("source review did not install", err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "proposal.publish_retry", "proposal_id": fresh.Proposal.ID, "expected_approval_version": fresh.Proposal.StateVersion}); err != nil {
		t.Fatal("publication retry failed", err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "proposal.publish_retry", "proposal_id": fresh.Proposal.ID, "expected_approval_version": fresh.Proposal.StateVersion + 1}); err == nil {
		t.Fatal("publication retry accepted different approval version")
	}
	_, err = skillManagementCall(t, c, map[string]any{"operation": "proposal.source_review", "proposal_id": p.ID, "expected_state_version": saved.StateVersion + 1, "request_key": "new-source"})
	var managementErr *localhost.ManagementError
	if !errors.As(err, &managementErr) || managementErr.Code != "conflict" {
		t.Fatal("stale source review accepted", err)
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
