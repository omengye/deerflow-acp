package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSkillLearningMaintenanceReadinessPreservesJobsAndSkipsBusyOwners(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	ready := false
	f.store.ready = func(_ context.Context, _ *sql.Tx, job Job) (bool, error) {
		return ready || job.Request.OwnerKey != strings.Repeat("e", 64), nil
	}
	var waiting []Job
	for i := range 19 {
		job, err := f.store.Enqueue(ctx, learningRequest("busy-"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		waiting = append(waiting, job)
	}
	next := learningRequest("ready-other-owner")
	next.OwnerKey = strings.Repeat("f", 64)
	other, err := f.store.Enqueue(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok || c.Job.ID != other.ID || c.Job.Attempts != 1 {
		t.Fatal("busy sources blocked another ready owner", c.Job, ok, err)
	}
	if err = f.store.Finish(ctx, c, "", false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract}); err != nil || ok {
			t.Fatal("busy source was claimed", ok, err)
		}
	}
	for _, before := range waiting {
		after, err := f.store.Get(ctx, before.ID)
		if err != nil || after != before {
			t.Fatal("waiting reset or consumed durable metadata", before, after, err)
		}
	}
	ready = true
	f.store.admit = func(context.Context, *sql.Tx, Job) error { return ErrConflict }
	c, ok, err = f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok || c.Job.ID != waiting[0].ID {
		t.Fatal("settled source did not claim the original job", c.Job, ok, err)
	}
	if _, err = f.store.Broker(c).Reserve(ctx, call("experience_extract")); !errors.Is(err, ErrConflict) {
		t.Fatal("readiness replaced paid authorization", err)
	}
	usage(t, f.store, 0, 0)
}

func TestSkillLearningMaintenanceReadinessKeepsOriginalRetryDeadline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	job, err := f.store.Enqueue(ctx, learningRequest("retry"))
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	r := reserve(t, f.store, c, "experience_extract")
	if err = f.store.Broker(c).Settle(ctx, r, Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, c, "model_failed", true); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Get(ctx, job.ID)
	if err != nil || before.State != "retry" || before.Attempts != 1 {
		t.Fatal(before, err)
	}
	f.store.ready = func(context.Context, *sql.Tx, Job) (bool, error) { return false, nil }
	f.now.Add(int64(3 * time.Second))
	if _, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract}); err != nil || ok {
		t.Fatal("busy retry consumed another attempt", ok, err)
	}
	after, err := f.store.Get(ctx, job.ID)
	if err != nil || after != before {
		t.Fatal("busy retry changed its original lease window", before, after, err)
	}
	terminals := 0
	f.store.terminal = func(_ context.Context, _ *sql.Tx, job Job) error {
		terminals++
		if job.State != "failed" || job.ErrorCode != "job_deadline" {
			t.Fatal("expired retry did not remain terminal", job)
		}
		return nil
	}
	f.now.Store(before.Deadline.Add(time.Second).UnixNano())
	if _, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract}); err != nil || ok {
		t.Fatal("readiness revived an expired job", ok, err)
	}
	after, err = f.store.Get(ctx, job.ID)
	if err != nil || after.State != "failed" || after.Attempts != 1 || after.Deadline != before.Deadline || terminals != 1 {
		t.Fatal(after, terminals, err)
	}
	usage(t, f.store, 1, 15)
}

func TestSkillLearningMaintenanceReadinessErrorDoesNotConsumeClaim(t *testing.T) {
	f := newFixture(t, policy())
	ctx := context.Background()
	job, err := f.store.Enqueue(ctx, learningRequest("storage-error"))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("readiness storage failure")
	f.store.ready = func(context.Context, *sql.Tx, Job) (bool, error) { return false, failure }
	if _, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract}); !errors.Is(err, failure) || ok {
		t.Fatal("storage failure silently consumed work", ok, err)
	}
	after, err := f.store.Get(ctx, job.ID)
	if err != nil || after != job {
		t.Fatal("failed readiness changed the durable claim", after, err)
	}
}

func TestSkillLearningMaintenanceReadinessCoversGlobalPendingCap(t *testing.T) {
	f := newFixture(t, policy())
	for i := range 99 {
		enqueue(t, f.store, "busy-"+string(rune('a'+i)))
	}
	ready := enqueue(t, f.store, "operator-ready")
	f.store.ready = func(_ context.Context, _ *sql.Tx, job Job) (bool, error) {
		return job.ID == ready.ID, nil
	}
	c := claim(t, f.store)
	if c.Job.ID != ready.ID {
		t.Fatal("bounded scan hid ready work behind busy automatic generation", c.Job)
	}
	var attempts int
	if err := f.db.DB().QueryRow(`SELECT sum(attempts) FROM df_skill_maintenance_jobs WHERE id<>?`, ready.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("bypassing busy jobs consumed their attempts", attempts, err)
	}
}
