package runtime

import (
	"context"
	"testing"
)

func TestFailedNewSkillNeedsFreshIndependentEvidenceForSuccessor(t *testing.T) {
	s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	first := experienceDiscoveries(t, e)
	if len(first) != 1 {
		t.Fatal("initial candidate missing")
	}
	g.extraInstruction = "Use token=GENERATED_PRIVATE_SUCCESSOR\n"
	if worked, err := e.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if j, err := e.GetGeneration(ctx, first[0].GenerationID); err != nil || j.State != "failed" {
		t.Fatal("fixture must fail quality", err)
	}
	if ds := experienceDiscoveries(t, e); len(ds) != 1 {
		t.Fatal("same evidence regenerated")
	}
	for _, task := range []string{"Third independent JSON artifact verification", "Fourth independent JSON artifact verification"} {
		consolidateExtract(t, s, x, task)
		consolidateRun(t, s)
	}
	experienceMatch(t, s)
	if _, err := s.Store.db.Exec(`UPDATE df_skill_discovery_cooldowns SET until_ns=0`); err != nil {
		t.Fatal(err)
	}
	g.extraInstruction = ""
	ds := experienceDiscoveries(t, e)
	if len(ds) != 2 {
		t.Fatal("fresh evidence blocked by lifetime reservation", len(ds))
	}
	var successors int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_successors`).Scan(&successors); err != nil || successors != 1 {
		t.Fatal("successor provenance missing", successors, err)
	}
	if ds := experienceDiscoveries(t, e); len(ds) != 2 {
		t.Fatal("fresh evidence spent twice")
	}
}
