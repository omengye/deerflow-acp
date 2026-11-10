package deerflow

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestOperatorDraftManualReviewIsRetired(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	ctx := context.Background()
	before, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"proposal.submit", "proposal.approve", "proposal.review_retry", "proposal.source_review"} {
		if _, err = skillManagementCall(t, c, map[string]any{"operation": operation}); err == nil {
			t.Fatal("retired operation accepted", operation)
		}
	}
	after, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil || after != before {
		t.Fatal("retired operation changed installed skill", after, err)
	}
	page, err := c.ListSkillProposals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Proposals) != 0 {
		t.Fatal("retired operation queued work", page, err)
	}
}

func TestOperatorDraftRejectsAmbiguousFiles(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"SKILL.md":null}`, `{"SKILL.md":"first","SKILL.md":"second"}`, `{"SKILL.md":{}}`, `{"SKILL.md":"a"} {}`} {
		if _, err := managementDraftFiles([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestOperatorNewPythonSkillCannotBypassExecutionWithNullBase(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	ctx := context.Background()
	p, err := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{
		SourceID: "project", Name: "new-python", Action: "create", RequestKey: "new-python",
		Reason: "A reviewed Python package with no installed base.",
		Files: map[string][]byte{
			"SKILL.md": []byte("---\nname: new-python\ndescription: New Python workflow\n---\nUse skill_run.\n"),
			"run.py":   []byte("print('reviewed source')\n"),
		},
	})
	if err != nil || p.Status != "pending_review" || !p.ScriptReviewRequired {
		t.Fatalf("new candidate: %+v %v", p, err)
	}
	value, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.get", "proposal_id": p.ID})
	if err != nil {
		t.Fatal(err)
	}
	projection := value.(map[string]any)
	if base, exists := projection["base_sha256"]; !exists || base != nil {
		t.Fatalf("new-skill base projection changed: %v", base)
	}
	request := map[string]any{"operation": "proposal.approve", "proposal_id": p.ID,
		"expected_state_version": p.StateVersion, "expected_base_sha256": projection["base_sha256"]}
	if _, err = skillManagementCall(t, c, request); err == nil {
		t.Fatal("new script skill published without risk acknowledgment")
	}
	request["acknowledge_unverified_scripts"] = true
	request["expected_state_version"] = p.StateVersion + 1
	if _, err = skillManagementCall(t, c, request); err == nil {
		t.Fatal("risk acknowledgment bypassed the reviewed state precondition")
	}
	request["expected_state_version"] = p.StateVersion
	if _, err = skillManagementCall(t, c, request); err == nil {
		t.Fatal("retired acknowledgment accepted")
	}
	published, err := c.GetSkillProposal(ctx, p.ID)
	if err != nil || published.Status == "published" || published.Approval != nil {
		t.Fatal("unexecuted scripts published", err)
	}
}
