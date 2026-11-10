package runtime

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"testing"
)

// Shared legacy fixture name; the policy now always requires two roots.
func enableExperienceReviewDrafts(t *testing.T, e *evolution.Service, _ bool) {
	enableExperienceDiscovery(t, e)
}

// The retired one-episode draft path must not silently return through a new
// discovery producer. The second independent root in the same chat is enough
// to generate a candidate, and it still cannot be manually adopted.
func TestSkillLearningSingleRootDoesNotGenerateManualDraft(t *testing.T) {
	for _, newSkill := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "new"}[newSkill], func(t *testing.T) {
			s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
			if newSkill {
				matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
			}
			consolidateExtract(t, s, x, "Inspect complete JSON input")
			consolidateRun(t, s)
			experienceMatch(t, s)
			enableExperienceDiscovery(t, e)
			if ds := experienceDiscoveries(t, e); len(ds) != 0 || len(g.seen) != 0 {
				t.Fatal("single root generated a draft", len(ds))
			}
			consolidateExtract(t, s, x, "Independently verify another JSON document")
			consolidateRun(t, s)
			experienceMatch(t, s)
			if ds := experienceDiscoveries(t, e); len(ds) != 1 || ds[0].ReviewDraft || len(ds[0].Evidence) != 2 {
				t.Fatal("independent evidence missing", len(ds))
			}
			p := experienceGenerate(t, e)
			if _, err := e.Approve(context.Background(), p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
				t.Fatal("manual adoption bypass")
			}
		})
	}
}
