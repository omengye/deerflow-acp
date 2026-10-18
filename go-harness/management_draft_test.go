package deerflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestOperatorDraftStagesFullPythonPackageAndRequiresAcknowledgment(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	ctx := context.Background()
	target, head, _, err := c.evolution.Head(ctx, "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"operation": "proposal.submit", "target": target, "expected_head": head, "request_key": "python-draft", "reason": "Keep Python source without implicit execution.", "files": map[string]string{"SKILL.md": "---\nname: research\ndescription: Research using Python\n---\nUse skill_run.\n", "run.py": "print('reviewed source')\n"}}
	value, err := skillManagementCall(t, c, request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	var result struct{ Proposal harness.SkillProposal }
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	p := result.Proposal
	if p.Status != "pending_review" || !p.ScriptReviewRequired || p.Evaluation.IsolatedScripts {
		t.Fatal("false execution claim", p)
	}
	current, _ := c.GetSkillHead(ctx, "project", "research")
	if current != head {
		t.Fatal("submission installed source")
	}
	if _, err = skillManagementCall(t, c, request); err != nil {
		t.Fatal("idempotency", err)
	}
	approve := map[string]any{"operation": "proposal.approve", "proposal_id": p.ID, "expected_state_version": p.StateVersion}
	if _, err = skillManagementCall(t, c, approve); err == nil {
		t.Fatal("silent script publication")
	}
	approve["acknowledge_unverified_scripts"] = "true"
	if _, err = skillManagementCall(t, c, approve); err == nil {
		t.Fatal("nonboolean ack")
	}
	approve["acknowledge_unverified_scripts"] = true
	if _, err = skillManagementCall(t, c, approve); err != nil {
		t.Fatal(err)
	}
	published, _ := c.GetSkillProposal(ctx, p.ID)
	if published.Status != "published" || !published.Approval.Review.AcknowledgeUnverifiedScripts {
		t.Fatal(published)
	}
	bad := target
	bad.SourceIdentity = "wrong"
	request["target"] = bad
	request["request_key"] = "foreign"
	if _, err = skillManagementCall(t, c, request); err == nil {
		t.Fatal("source identity ignored")
	}
}

func TestOperatorDraftRejectsAmbiguousFiles(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"SKILL.md":null}`, `{"SKILL.md":"first","SKILL.md":"second"}`, `{"SKILL.md":{}}`, `{"SKILL.md":"a"} {}`} {
		if _, err := managementDraftFiles([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestOperatorNewPythonSkillPublishesOnceWithExplicitNullBase(t *testing.T) {
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
	result, err := skillManagementCall(t, c, request)
	if err != nil {
		t.Fatal("single explicit approval failed", err)
	}
	replayed, err := skillManagementCall(t, c, request)
	if err != nil || result.(map[string]any)["catalog_version"] != replayed.(map[string]any)["catalog_version"] {
		t.Fatal("duplicate click republished or lost the original decision", err)
	}
	published, err := c.GetSkillProposal(ctx, p.ID)
	if err != nil || published.Status != "published" || published.Approval == nil || !published.Approval.Review.AcknowledgeUnverifiedScripts {
		t.Fatalf("approval evidence missing: %+v %v", published, err)
	}
}
