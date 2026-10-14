package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningAutoPatchFrontmatterRemainsManual(t *testing.T) {
	base := []byte("---\nname: json-check\ndescription: Inspect JSON\n---\nOriginal body\n")
	for _, test := range []struct {
		name, candidate string
		allowed         bool
	}{
		{"body", "---\nname: json-check\ndescription: Inspect JSON\n---\nImproved body\n", true},
		{"crlf", "---\r\nname: json-check\r\ndescription: Inspect JSON\r\n---\r\nImproved body\r\n", true},
		{"description", "---\nname: json-check\ndescription: Inspect all JSON\n---\nImproved body\n", false},
		{"compatibility", "---\nname: json-check\ndescription: Inspect JSON\ncompatibility: Windows\n---\nImproved body\n", false},
		{"format-only", "---\nname: json-check\ndescription: 'Inspect JSON'\n---\nImproved body\n", false},
		{"missing", "Improved body\n", false},
		{"unclosed", "---\nname: json-check\ndescription: Inspect JSON\nImproved body\n", false},
		{"oversized", "---\nname: json-check\ndescription: Inspect JSON" + strings.Repeat(" ", 16<<10) + "\n---\nImproved body\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameAutoPatchFrontmatter(base, []byte(test.candidate)); got != test.allowed {
				t.Fatal("frontmatter admission", got, test.allowed)
			}
		})
	}
}

func TestSkillLearningAutoPatchRequiresTargetedAssertionImprovement(t *testing.T) {
	for _, mode := range []string{"targeted", "unchanged", "regression-only", "baseline-timeout", "regression-timeout", "regressed", "failed-report", "no-regression-cases"} {
		t.Run(mode, func(t *testing.T) {
			targeted := harness.SkillBehaviorComparison{Kind: "targeted", BaseErrorCode: "assertion_failed", CandidatePassed: true}
			regression := harness.SkillBehaviorComparison{Kind: "regression", BasePassed: true, CandidatePassed: true}
			report := harness.SkillBehaviorReport{State: "passed", TargetedCases: 1, RegressionCases: 1}
			switch mode {
			case "unchanged", "regression-only":
				targeted.BasePassed, targeted.BaseErrorCode = true, ""
				if mode == "regression-only" {
					regression.BasePassed, regression.BaseErrorCode = false, "assertion_failed"
				}
			case "baseline-timeout":
				targeted.BaseErrorCode = "timeout"
			case "regression-timeout":
				regression.BasePassed, regression.BaseErrorCode = false, "model_error"
			case "regressed":
				regression.CandidatePassed, regression.CandidateErrorCode = false, "assertion_failed"
			case "failed-report":
				report.State = "failed"
			case "no-regression-cases":
				report.RegressionCases = 0
			}
			report.Results = []harness.SkillBehaviorComparison{targeted, regression}
			// A counter alone is never evidence of a targeted improvement.
			report.Improvements = 1
			if got := learningAutoPatchImproved(report); got != (mode == "targeted") {
				t.Fatal("targeted admission", got, report)
			}
		})
	}
}

func TestSkillLearningAutoPatchRequiresIndependentHostGrant(t *testing.T) {
	f, _, _, _ := behaviorFixture(t)
	o := harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: true, MaxChangedLines: 40, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
	if err := f.service.ConfigureAutoPatch(o); err == nil {
		t.Fatal("learning automatic publication configured without learning/discovery authority")
	}
	o.ExperiencePatternsEnabled = false
	if err := f.service.ConfigureAutoPatch(o); err != nil {
		t.Fatal("existing auto_patch unexpectedly requires learning", err)
	}
	if err := f.service.guardAutoPatch(context.WithValue(context.Background(), autoPatchContextKey{}, true), nil, Proposal{Trigger: Trigger{Type: "experience_pattern"}}, ValidationInput{}); !errors.Is(err, errAutoPatchReview) {
		t.Fatal("old auto_patch grant admitted learned candidate", err)
	}
}

func TestSkillLearningAutoPatchRetainsManualPackageBoundaries(t *testing.T) {
	for _, mode := range []string{"new", "imported", "disabled", "overlay", "support-file", "manual-origin", "no-behavior"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, _ := behaviorFixture(t)
			// Isolate the structural gate. Full host authority and publication
			// transaction are covered by the runtime learning fixtures.
			f.service.autoPatch = &autoPatchHost{options: harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: true}}
			f.service.discovery = &discoveryHost{options: harness.SkillDiscoveryOptions{ExperiencePatternsEnabled: true}}
			p := Proposal{Trigger: Trigger{Type: "experience_pattern"}, Base: harness.SkillHead{Exists: true, Enabled: true, Managed: true}, Origin: "automatic", Action: "edit", ChangedFiles: []string{"SKILL.md"}, Evaluation: Report{Security: "passed", Behavior: "passed", BehaviorReport: &harness.SkillBehaviorReport{TargetedCases: 1, RegressionCases: 1}}}
			switch mode {
			case "new":
				p.Base, p.Action = harness.SkillHead{}, "create"
			case "imported":
				p.Base.Managed = false
			case "disabled":
				p.Base.Enabled = false
			case "overlay":
				p.OverlayBase = &harness.SkillRef{Version: strings.Repeat("a", 64)}
			case "support-file":
				p.ChangedFiles = []string{"SKILL.md", "references/notes.txt"}
			case "manual-origin":
				p.Origin = "manual"
			case "no-behavior":
				p.Evaluation.Behavior, p.Evaluation.BehaviorReport = "not_run", nil
			}
			if err := f.service.guardAutoPatch(context.WithValue(context.Background(), autoPatchContextKey{}, true), nil, p, ValidationInput{}); !errors.Is(err, errAutoPatchReview) {
				t.Fatal("learning publication bypassed manual boundary", mode, err)
			}
		})
	}
}

func TestSkillLearningAutoPatchSourceRecordIsBoundedAndHistorical(t *testing.T) {
	source := harness.SkillAutoPatchLearningSource{GenerationID: "smj_fixture", DiscoveryID: "sd_fixture", DiscoverySHA: strings.Repeat("a", 64), PatternID: "lpat_fixture", PatternRevision: 2, MatchID: "lmatch_fixture", PolicyIdentity: strings.Repeat("b", 64), EvidenceSHA: strings.Repeat("c", 64), SourceState: "valid_at_publication"}
	for _, mode := range []string{"historical", "withdrawn", "unavailable", "old-record", "not-authorized", "quality-claim", "invalid-digest", "empty-match", "zero-revision", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			x := source
			o := harness.SkillAutoPatchObservation{Options: harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: true}, LearningSource: &x}
			switch mode {
			case "withdrawn", "unavailable":
				x.SourceState = mode
			case "old-record":
				o.LearningSource, o.Options.ExperiencePatternsEnabled = nil, false
			case "not-authorized":
				o.Options.ExperiencePatternsEnabled = false
			case "quality-claim":
				x.SourceState = "quality_passed"
			case "invalid-digest":
				x.EvidenceSHA = "fixture"
			case "empty-match":
				x.MatchID = ""
			case "zero-revision":
				x.PatternRevision = 0
			case "oversized":
				x.PatternID = strings.Repeat("x", 129)
			}
			want := mode == "historical" || mode == "withdrawn" || mode == "unavailable" || mode == "old-record"
			if validAutoPatchLearningSource(o) != want {
				t.Fatal("learning source record admission", mode, o)
			}
		})
	}
}
