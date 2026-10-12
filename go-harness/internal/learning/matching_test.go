package learning

import (
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestSkillLearningMatchingDecisionsCannotInventTargetsOrCapabilities(t *testing.T) {
	input := harness.SkillExperienceMatchInput{InventoryComplete: true, Skills: []harness.SkillExperienceMatchChoice{{ID: "allowed"}}}
	for _, mode := range []string{"body_gap", "trigger_mismatch", "already_covered", "new_capability", "irrelevant", "ambiguous", "foreign", "unknown", "untargeted", "incomplete"} {
		d := harness.SkillExperienceMatchDecision{Relation: mode, Reason: "Boundary check token=PRIVATE_MATCH_REASON"}
		view := input
		switch mode {
		case "body_gap", "trigger_mismatch", "already_covered":
			d.SkillID = "allowed"
		case "foreign":
			d.Relation, d.SkillID = "body_gap", "unauthorized"
		case "untargeted":
			d.Relation, d.SkillID = "ambiguous", "allowed"
		case "incomplete":
			d.Relation, view.InventoryComplete = "new_capability", false
		}
		got, err := validateMatchDecision(view, d)
		want := mode != "foreign" && mode != "unknown" && mode != "untargeted" && mode != "incomplete"
		if (err == nil) != want || want && strings.Contains(got.Reason, "PRIVATE_MATCH_REASON") {
			t.Fatal(mode, got, err)
		}
	}
}

func TestSkillLearningMatchingProjectionKeepsAttributionSeparate(t *testing.T) {
	target := harness.SkillTarget{SourceID: "source", SourceIdentity: strings.Repeat("a", 64), Name: "sample", Scope: harness.SkillScopeWorkspace}
	head := harness.SkillHead{Exists: true, Enabled: true, Version: strings.Repeat("b", 64), StateVersion: 2}
	old := harness.SkillRef{SourceID: target.SourceID, SourceIdentity: target.SourceIdentity, Name: target.Name, Version: strings.Repeat("c", 64), Hash: strings.Repeat("c", 64)}
	snap := matchSnapshot{ID: "match", Session: "session", Pattern: patternDependency{ID: "pattern", Revision: 3}, View: harness.SkillExperienceMatchInput{EvidenceState: "insufficient_evidence"}, Choices: []matchChoice{{ID: "choice", Skill: skills.MatchSkill{Target: target, Head: head}, ActualReads: []harness.SkillRef{old}}}}
	d := harness.SkillExperienceMatchDecision{Relation: "body_gap", SkillID: "choice", Reason: "Concrete missing instruction"}
	got, err := planMatch(snap, d, time.Now())
	if err != nil || got.Target == nil || *got.Target != target || got.Head == nil || *got.Head != head || len(got.ActualReads) != 1 || got.ActualReads[0] != old || got.EvidenceState != "insufficient_evidence" {
		t.Fatal(got, err)
	}
	got.ActualReads[0].Version = "mutated"
	if snap.Choices[0].ActualReads[0] != old {
		t.Fatal("projection changed fixed attribution")
	}
	snap.Choices[0].ActualReads = []harness.SkillRef{}
	got, err = planMatch(snap, d, time.Now())
	if err != nil || len(got.ActualReads) != 0 {
		t.Fatal("semantic relatedness fabricated a read", got, err)
	}
}

func TestSkillLearningMatchingCapabilityNameIsOnlyAReadableSuggestion(t *testing.T) {
	input := harness.SkillExperienceMatchInput{InventoryComplete: true, Skills: []harness.SkillExperienceMatchChoice{}}
	for _, name := range []string{"structured-json-audit", "powershell-config-validation", "sample", "workflow-0123456789abcdef", "config-12345", "../new-skill", "New-Skill", "secret_key", "json--audit", "a-" + strings.Repeat("b", 55)} {
		d := harness.SkillExperienceMatchDecision{Relation: "new_capability", Reason: "A reusable procedure not covered", SuggestedName: name}
		got, err := validateMatchDecision(input, d)
		want := name == "structured-json-audit" || name == "powershell-config-validation"
		if (err == nil) != want {
			t.Fatal(name, got, err)
		}
		if want {
			snap := matchSnapshot{ID: "match", Session: "session", Pattern: patternDependency{ID: "pattern", Revision: 1}}
			m, err := planMatch(snap, got, time.Now())
			if err != nil || m.SuggestedName != name || m.Target != nil || m.Head != nil || m.OverlayBase != nil || len(m.ActualReads) != 0 {
				t.Fatal("suggestion granted source or runtime read", m, err)
			}
		}
	}
	if _, err := validateMatchDecision(input, harness.SkillExperienceMatchDecision{Relation: "ambiguous", Reason: "No precise target", SuggestedName: "structured-json-audit"}); err == nil {
		t.Fatal("non-capability decision smuggled a creation name")
	}
	// Old/custom matcher output can still be shadow history, never promotion.
	if _, err := validateMatchDecision(input, harness.SkillExperienceMatchDecision{Relation: "new_capability", Reason: "Legacy shadow decision"}); err != nil {
		t.Fatal(err)
	}
}
