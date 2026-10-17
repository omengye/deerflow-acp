package evolution

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func validateTriggerResult(r harness.SkillBehaviorResult, in harness.SkillBehaviorInput) error {
	if !skillbehavior.TriggerCase(in.Case) {
		if r.Selection != nil {
			return invalid("execution result cannot claim metadata-only selection")
		}
		return nil
	}
	if r.ToolCalls != 0 {
		return invalid("trigger selection cannot execute tools")
	}
	x := r.Selection
	if x == nil {
		if r.Passed || r.StopReason != "failed" || r.ErrorCode == "" || r.ErrorCode == "assertion_failed" {
			return invalid("trigger result requires a completed selection observation")
		}
		return nil
	}
	if x.RuleVersion != skillbehavior.SelectionRuleVersion || x.TargetName != in.SkillName || !hexDigest(x.CatalogDigest) || r.StopReason != "completed" || r.ModelCalls < 1 {
		return invalid("trigger observation identity or outcome is invalid")
	}
	member := x.SelectedName == "" || in.Version != "" && x.SelectedName == in.SkillName
	for _, sk := range in.Case.TriggerSkills {
		member = member || x.SelectedName == sk.Name
	}
	if !member || len(in.Case.Assertions) != 1 || len(r.Checks) != 1 {
		return invalid("trigger decision is outside the host catalog")
	}
	want := skillbehavior.SelectionCheck(in.Case.Assertions[0], *x)
	code := "assertion_failed"
	if want.Passed {
		code = ""
	}
	if r.Checks[0] != want || r.Passed != want.Passed || r.ErrorCode != code || r.OutputDigest != want.ActualDigest {
		return invalid("trigger grading does not match the observed decision")
	}
	return nil
}

func validateTriggerCatalog(r harness.SkillBehaviorResult, in harness.SkillBehaviorInput) error {
	if r.Selection == nil {
		return nil
	}
	_, digest, err := skillbehavior.SelectionCatalog(in)
	if err != nil || r.Selection.CatalogDigest != digest {
		return invalid("trigger result does not match the immutable description catalog")
	}
	return nil
}

// Read through the existing transaction, never the Registry/pool. Admission
// reconstructs each catalog from immutable variant bytes and the fixed suite.
func validateTriggerCatalogTx(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, r harness.SkillBehaviorResult, in harness.SkillBehaviorInput) error {
	if r.Selection == nil {
		return nil
	}
	in.Files = map[string][]byte{}
	if in.Version != "" {
		data, err := skills.ReadStoredFile(ctx, tx, target.SourceIdentity, target.Name, in.Version, "SKILL.md", 512<<10)
		if err != nil {
			return err
		}
		in.Files["SKILL.md"] = data
	}
	return validateTriggerCatalog(r, in)
}
