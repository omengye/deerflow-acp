package skillmaintenance

import (
	"context"
	"testing"
)

func TestMaintenanceDownstreamPriorityRetainsFIFOService(t *testing.T) {
	f := newFixture(t, policy())
	ctx := context.Background()
	old, err := f.store.Enqueue(ctx, learningRequest("old-extract"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []JobKind{ExperienceConsolidate, SkillMatch} {
		r := learningRequest(string(kind))
		r.Kind = kind
		if _, err = f.store.Enqueue(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	kinds := []JobKind{ExperienceExtract, ExperienceConsolidate, SkillMatch, SkillGeneration}
	for i := 0; i < 4; i++ {
		if i >= 2 {
			enqueue(t, f.store, "generation-"+string(rune('a'+i)))
		}
		c, ok, err := f.store.ClaimKinds(ctx, kinds)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		if c.Job.ID == old.ID {
			t.Fatal("upstream ran before downstream")
		}
		if err = f.store.Finish(ctx, c, "", false); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(t, f.store, "new-generation")
	c, ok, err := f.store.ClaimKinds(ctx, kinds)
	if err != nil || !ok || c.Job.ID != old.ID {
		t.Fatal("FIFO share starved extraction", c.Job, ok, err)
	}
}
