package evolution

import (
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func TestSkillTriggerAutoPatchAllowsOnlyDescriptionSpan(t *testing.T) {
	base := "---\nname: json-check\ndescription: Inspect JSON\ncompatibility: Windows\nmetadata:\n  boundary: local\n---\nBody\n"
	for _, tc := range []struct {
		name, candidate string
		allowed, body   bool
	}{
		{"plain", strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), true, false},
		{"crlf", strings.ReplaceAll(strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), "\n", "\r\n"), true, false},
		{"block", strings.Replace(base, "description: Inspect JSON", "description: >-\n  Inspect structured\n  JSON", 1), true, false},
		{"body", strings.Replace(strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), "Body", "Improved body", 1), true, true},
		{"format-only", strings.Replace(base, "description: Inspect JSON", "description: 'Inspect JSON'", 1), false, false},
		{"compatibility", strings.Replace(base, "Windows", "Linux", 1), false, false},
		{"metadata", strings.Replace(strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), "local", "remote", 1), false, false},
		{"rename", strings.Replace(strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), "name: json-check", "name: another-check", 1), false, false},
		{"other-format", strings.Replace(strings.Replace(base, "Inspect JSON", "Inspect structured JSON", 1), "compatibility: Windows", "compatibility: 'Windows'", 1), false, false},
		{"permission", strings.Replace(base, "compatibility: Windows", "allowed-tools: bash", 1), false, false},
		{"missing", "Body\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, allowed := autoPatchDescriptionChange([]byte(base), []byte(tc.candidate))
			if allowed != tc.allowed || allowed && body != tc.body {
				t.Fatal(body, allowed, tc)
			}
		})
	}
}

func triggerPassingReport() harness.SkillBehaviorReport {
	selection := func(candidate bool, selected string) *harness.SkillBehaviorSelection {
		digest := strings.Repeat("a", 64)
		if candidate {
			digest = strings.Repeat("b", 64)
		}
		return &harness.SkillBehaviorSelection{RuleVersion: skillbehavior.SelectionRuleVersion, TargetName: "json-check", SelectedName: selected, CatalogDigest: digest}
	}
	return harness.SkillBehaviorReport{RuleVersion: behaviorTriggerRuleVersion, State: "passed", TargetedCases: 1, RegressionCases: 1, TriggerPositiveCases: 1, TriggerNegativeCases: 1, Results: []harness.SkillBehaviorComparison{
		{Kind: "targeted", BasePassed: true, CandidatePassed: true},
		{Kind: "regression", BasePassed: true, CandidatePassed: true},
		{Kind: "trigger_positive", BaseErrorCode: "assertion_failed", CandidatePassed: true, BaseSelection: selection(false, ""), CandidateSelection: selection(true, "json-check")},
		{Kind: "trigger_negative", BasePassed: true, CandidatePassed: true, BaseSelection: selection(false, ""), CandidateSelection: selection(true, "")},
	}}
}

func TestSkillTriggerAutoPatchRequiresPositiveNegativeCoverageAndImprovement(t *testing.T) {
	base := []byte("---\nname: json-check\ndescription: Inspect JSON\n---\nBody\n")
	candidate := []byte(strings.Replace(string(base), "Inspect JSON", "Inspect structured JSON", 1))
	for _, mode := range []string{"description", "body-and-trigger", "body-without-improvement", "no-improvement", "no-positive", "no-negative", "old-report", "regressed", "baseline-timeout", "missing-observation", "same-catalog", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			r := triggerPassingReport()
			c := append([]byte{}, candidate...)
			switch mode {
			case "body-and-trigger", "body-without-improvement":
				c = []byte(strings.Replace(string(c), "Body", "Improved body", 1))
				if mode == "body-and-trigger" {
					r.Results[0].BasePassed, r.Results[0].BaseErrorCode = false, "assertion_failed"
				}
			case "no-improvement":
				r.Results[2].BasePassed, r.Results[2].BaseErrorCode = true, ""
			case "no-positive":
				r.TriggerPositiveCases = 0
			case "no-negative":
				r.TriggerNegativeCases = 0
			case "old-report":
				r.RuleVersion = behaviorRuleVersion
			case "regressed":
				r.Results[3].CandidatePassed, r.Results[3].CandidateErrorCode = false, "assertion_failed"
			case "baseline-timeout":
				r.Results[2].BaseErrorCode = "timeout"
			case "missing-observation":
				r.Results[2].BaseSelection = nil
			case "same-catalog":
				r.Results[2].CandidateSelection.CatalogDigest = r.Results[2].BaseSelection.CatalogDigest
			case "metadata":
				c = []byte(strings.Replace(string(c), "name: json-check", "name: another-check", 1))
			}
			if got := learningAutoPatchContentAllowed(base, c, r); got != (mode == "description" || mode == "body-and-trigger") {
				t.Fatal("description/body authorization", mode, got)
			}
		})
	}
}
