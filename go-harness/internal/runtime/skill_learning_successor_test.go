package runtime

import (
	"context"
	"testing"
)

func TestSkillLearningFreshEvidenceWaitsForActiveCandidate(t *testing.T) {
	s, x, _, e, generator, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	original := experienceDiscoveries(t, e)
	if len(original) != 1 {
		t.Fatal(original)
	}
	p := experienceGenerate(t, e)
	for range 2 {
		consolidateExtract(t, s, x, "Independent complete JSON inspection")
		consolidateRun(t, s)
		experienceMatch(t, s)
	}
	// New evidence remains in the ledger while one candidate owns this target.
	// Neither a cooldown change nor a restart scan creates a competing proposal.
	if _, err := s.Store.db.Exec(`UPDATE df_skill_discovery_cooldowns SET until_ns=0`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if ds := experienceDiscoveries(t, e); len(ds) != 1 {
			t.Fatal("parallel candidate bypassed target ownership", ds)
		}
	}
	retained, err := e.Get(ctx, p.ID)
	if err != nil || retained.Status != "pending_review" || retained.StateVersion != p.StateVersion || len(generator.seen) != 1 {
		t.Fatal(retained, err)
	}
	var consumed, members int
	if err := s.Store.db.QueryRow(`SELECT (SELECT count(*) FROM df_skill_discovery_episode_consumed),count(*) FROM df_learning_pattern_members`).Scan(&consumed, &members); err != nil || consumed != 2 || members != 4 {
		t.Fatal("waiting dropped or reused fresh evidence", consumed, members, err)
	}
}
