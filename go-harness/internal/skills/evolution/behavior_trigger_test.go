package evolution

import (
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func TestSkillTriggerHostReconstructsSelectionAndOracleInsteadOfTrustingPassed(t *testing.T) {
	in := harness.SkillBehaviorInput{EvaluationID: "sbe_fixture", CaseID: "positive", Variant: "candidate", Version: strings.Repeat("b", 64), SkillName: "json-check", Repetition: 1, Files: map[string][]byte{"SKILL.md": []byte("---\nname: json-check\ndescription: Inspect structured JSON\n---\nBody\n")}, Case: harness.SkillBehaviorCase{ID: "positive", Kind: "trigger_positive", Prompt: "Check configuration", Repetitions: 1, Assertions: []harness.SkillBehaviorAssertion{{ID: "chosen", Kind: "skill_selected", Count: 1}}}}
	_, digest, err := skillbehavior.SelectionCatalog(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "forged-pass", "outside-catalog", "wrong-target", "wrong-rule", "missing-selection", "execution", "different-description", "operational-failure"} {
		t.Run(mode, func(t *testing.T) {
			x := &harness.SkillBehaviorSelection{RuleVersion: skillbehavior.SelectionRuleVersion, TargetName: in.SkillName, SelectedName: in.SkillName, CatalogDigest: digest}
			check := skillbehavior.SelectionCheck(in.Case.Assertions[0], *x)
			r := harness.SkillBehaviorResult{CaseID: in.CaseID, Variant: in.Variant, Version: in.Version, Repetition: 1, Passed: true, StopReason: "completed", OutputDigest: check.ActualDigest, TraceDigest: digest, Checks: []harness.SkillBehaviorCheck{check}, ModelCalls: 1, Selection: x}
			switch mode {
			case "forged-pass":
				x.SelectedName = ""
			case "outside-catalog":
				x.SelectedName = "private-tool"
			case "wrong-target":
				x.TargetName = "another-name"
			case "wrong-rule":
				x.RuleVersion = "execution-pass"
			case "missing-selection":
				r.Selection = nil
			case "execution":
				r.ToolCalls = 1
			case "different-description":
				x.CatalogDigest = strings.Repeat("f", 64)
			case "operational-failure":
				r.Selection, r.Passed, r.StopReason, r.ErrorCode = nil, false, "failed", "timeout"
			}
			err := validateBehaviorResult(r, in)
			if err == nil {
				err = validateTriggerCatalog(r, in)
			}
			if (err == nil) != (mode == "valid" || mode == "operational-failure") {
				t.Fatal("host trusted forged selection grading", mode, err)
			}
		})
	}
}
