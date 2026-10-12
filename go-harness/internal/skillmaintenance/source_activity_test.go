package skillmaintenance

import (
	"context"
	"testing"
	"time"
)

func TestSkillLearningSourceActivityRetainsOriginalAttemptAndDeadlineLimits(t *testing.T) {
	for _, stop := range []string{"attempts", "deadline"} {
		t.Run(stop, func(t *testing.T) {
			f := newFixture(t, policy())
			ctx := context.Background()
			job, err := f.store.Enqueue(ctx, learningRequest("source-active"))
			if err != nil {
				t.Fatal(err)
			}
			attempts := 3
			if stop == "deadline" {
				attempts = 1
			}
			var deadline time.Time
			for i := 1; i <= attempts; i++ {
				claim, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
				if err != nil || !ok || claim.Job.Attempts != i {
					t.Fatal(claim.Job, ok, err)
				}
				if deadline.IsZero() {
					deadline = claim.Job.Deadline
				} else if deadline != claim.Job.Deadline {
					t.Fatal("temporary activity extended the original deadline", claim.Job)
				}
				receipt := reserve(t, f.store, claim, "experience_extract")
				if err := f.store.Broker(claim).Settle(ctx, receipt, Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
					t.Fatal(err)
				}
				if err := f.store.Finish(ctx, claim, "source_active", true); err != nil {
					t.Fatal(err)
				}
				f.now.Add(int64(5 * time.Second))
			}
			if stop == "deadline" {
				f.now.Store(deadline.Add(time.Second).UnixNano())
			}
			if _, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract}); err != nil || ok {
				t.Fatal("source activity revived exhausted work", ok, err)
			}
			final, err := f.store.Get(ctx, job.ID)
			code := "source_active"
			if stop == "deadline" {
				code = "job_deadline"
			}
			if err != nil || final.State != "failed" || final.Attempts != attempts || final.Deadline != deadline || final.ErrorCode != code || final.Request != job.Request {
				t.Fatal("source activity bypassed bounded recovery", final, err)
			}
			usage(t, f.store, int64(attempts), int64(15*attempts))
		})
	}
}
