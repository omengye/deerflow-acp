package skillmaintenance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func learningRequest(key string) Request {
	return Request{Kind: ExperienceExtract, OwnerKind: WorkspaceOwner, OwnerKey: strings.Repeat("e", 64), Key: key, Reference: "extract/" + key, InputDigest: strings.Repeat("b", 64)}
}

func TestTypedMaintenanceExpiredOtherProducerCannotHoldSlot(t *testing.T) {
	p := policy()
	p.Workers = 1
	f := newFixture(t, p)
	if _, err := f.store.Enqueue(context.Background(), learningRequest("expired")); err != nil {
		t.Fatal(err)
	}
	l, ok, err := f.store.ClaimKinds(context.Background(), []JobKind{ExperienceExtract})
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	enqueue(t, f.store, "foreground-maintenance")
	f.now.Add(int64(leaseDuration + time.Second))
	g := claim(t, f.store)
	if g.Job.Request.Kind != SkillGeneration {
		t.Fatal("expired unconfigured producer blocked generation")
	}
	if err = f.store.Heartbeat(context.Background(), l); err != ErrConflict {
		t.Fatal("expired learning claim revived", err)
	}
}

func TestTypedMaintenanceLearningBackpressureKeepsSkillSlots(t *testing.T) {
	f := newFixture(t, policy())
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		r := learningRequest("backlog-" + string(rune('a'+i)))
		if i%2 == 1 {
			r.Kind = SkillMatch
		}
		if _, err := f.store.Enqueue(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Enqueue(ctx, learningRequest("overflow")); !errors.Is(err, ErrBudget) {
		t.Fatal("learning inventory exceeded cap", err)
	}
	enqueue(t, f.store, "manual-generation")
	if c := claim(t, f.store); c.Job.Request.Kind != SkillGeneration {
		t.Fatal("backlog blocked explicit generation", c.Job)
	}
}

func TestTypedMaintenanceScopeNamespaceAndProducerIsolation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	r := learningRequest("same")
	first, err := f.store.Enqueue(ctx, r)
	if err != nil || first.Request != r || first.Request.Name != "" || first.Request.SourceIdentity != "" {
		t.Fatalf("unbound learning: %+v %v", first, err)
	}
	if replay, err := f.store.Enqueue(ctx, r); err != nil || replay.ID != first.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	other := r
	other.OwnerKey = strings.Repeat("f", 64)
	if next, err := f.store.Enqueue(ctx, other); err != nil || next.ID == first.ID {
		t.Fatalf("scope collision: %+v %v", next, err)
	}
	other = r
	other.Kind = SkillMatch
	if next, err := f.store.Enqueue(ctx, other); err != nil || next.ID == first.ID {
		t.Fatalf("kind collision: %+v %v", next, err)
	}
	other = r
	other.InputDigest = strings.Repeat("c", 64)
	if _, err := f.store.Enqueue(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed input: %v", err)
	}
	if _, found, err := f.store.Claim(ctx); err != nil || found {
		t.Fatalf("generation worker stole learning: %v %v", found, err)
	}
	generation := enqueue(t, f.store, "same")
	g := claim(t, f.store)
	if g.Job.ID != generation.ID || g.Job.Request.Kind != SkillGeneration {
		t.Fatal(g.Job)
	}
	c, found, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !found || c.Job.ID != first.ID {
		t.Fatalf("learning claim: %+v %v %v", c.Job, found, err)
	}
	if _, err = f.store.Broker(c).Reserve(ctx, call("generation")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("learning borrowed generation stage: %v", err)
	}
	if _, err = f.store.Broker(g).Reserve(ctx, call("experience_extract")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("generation borrowed learning stage: %v", err)
	}
	lr := reserve(t, f.store, c, "experience_extract")
	if err = f.store.Broker(c).Settle(ctx, lr, Usage{}); err != nil {
		t.Fatal(err)
	}
	reserve(t, f.store, g, "generation")
	usage(t, f.store, 2, 600)
}

func TestTypedMaintenanceRejectsTargetsAndUnknownProducers(t *testing.T) {
	f := newFixture(t, policy())
	for _, change := range []func(*Request){
		func(r *Request) { r.Kind = "invented" },
		func(r *Request) { r.Kind = LegacyJob },
		func(r *Request) { r.OwnerKind = SkillOwner },
		func(r *Request) { r.OwnerKey = "raw workspace" },
		func(r *Request) { r.Name = "fake-skill" },
		func(r *Request) { r.SourceIdentity = strings.Repeat("a", 64) },
	} {
		r := learningRequest("bad")
		change(&r)
		if _, err := f.store.Enqueue(context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid request %+v: %v", r, err)
		}
	}
}

func TestTypedMaintenanceLegacyBindingPreservesLeaseAndCharges(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	j := enqueue(t, f.store, "legacy")
	c := claim(t, f.store)
	reserve(t, f.store, c, "generation")
	if _, err := f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET job_kind='legacy',owner_key='' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	old, token, lease, err := readJob(ctx, f.db.DB(), j.ID)
	if err != nil || old.Request.Kind != LegacyJob {
		t.Fatalf("legacy read: %+v %v", old, err)
	}
	if _, err := f.store.Broker(c).Reserve(ctx, call("quality")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbound legacy spent: %v", err)
	}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = BindLegacyKindTx(ctx, tx, old, SkillGeneration); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fresh, nextToken, nextLease, err := readJob(ctx, f.db.DB(), j.ID)
	old.Request.Kind = SkillGeneration
	if err != nil || fresh != old || token != nextToken || lease != nextLease {
		t.Fatalf("migration reset execution: %+v %v", fresh, err)
	}
	usage(t, f.store, 1, 300)
	unknown := enqueue(t, f.store, "orphan")
	if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET job_kind='legacy' WHERE id=?`, unknown.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.Claim(ctx); err != nil || found {
		t.Fatalf("unknown legacy claimed: %v %v", found, err)
	}
}
