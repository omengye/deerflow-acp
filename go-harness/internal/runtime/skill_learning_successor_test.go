package runtime

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningReviewDraftSuccessorUsesFreshEvidence(t *testing.T) {
	s, x, _, e, _, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "First complete JSON inspection")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceReviewDrafts(t, e, false)
	original := experienceDiscoveries(t, e)
	if len(original) != 1 {
		t.Fatal(original)
	}
	p := experienceGenerate(t, e)
	status, err := e.AutoPatchStatus(ctx)
	if err != nil || status.Readiness.NeedsAttention != 0 || status.Readiness.WaitingCandidates != 1 {
		t.Fatal(status, err)
	}
	for i := range 2 {
		if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
			t.Fatal(err)
		}
		consolidateExtract(t, s, x, "Independent complete JSON inspection")
		consolidateRun(t, s)
		experienceMatch(t, s)
		if i == 0 && len(experienceDiscoveries(t, e)) != 1 {
			t.Fatal("reused consumed evidence")
		}
	}
	if len(experienceDiscoveries(t, e)) != 1 {
		t.Fatal("bypassed cooldown")
	}
	// Expire only the fixture cooldown, then inject an admission failure.
	if _, err = s.Store.db.Exec(`UPDATE df_skill_discovery_cooldowns SET until_ns=0;
 CREATE TRIGGER successor_fault BEFORE INSERT ON df_skill_generation_inputs BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _ = e.RunDiscovery(ctx)
	}
	unchanged, err := e.Get(ctx, p.ID)
	if err != nil || unchanged.Status != "pending_review" || unchanged.StateVersion != p.StateVersion {
		t.Fatal("failed admission changed draft", unchanged, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER successor_fault`); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 2 {
		t.Fatal(ds)
	}
	var next harness.SkillDiscovery
	for _, d := range ds {
		if d.ID != original[0].ID {
			next = d
		}
	}
	if next.ReviewDraft || next.PreviousProposalID != p.ID || len(next.Evidence) != 2 {
		t.Fatal(next)
	}
	for _, ref := range next.Evidence {
		if ref.EpisodeKey == original[0].Evidence[0].EpisodeKey {
			t.Fatal("reused original episode")
		}
	}
	old, err := e.Get(ctx, p.ID)
	if err != nil || old.Status != "stale" || old.StateVersion != p.StateVersion+1 {
		t.Fatal(old, err)
	}
	var consumed int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed); err != nil || consumed != 3 {
		t.Fatal(consumed, err)
	}
	if len(experienceDiscoveries(t, e)) != 2 {
		t.Fatal("duplicated successor")
	}
}
