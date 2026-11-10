package evolution

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
)

func TestAutomaticEnvironmentChangeReusesCandidateAndArchivesVerdict(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := behaviorFixture(t)
	j := queueGeneration(t, f, "environment")
	generationRun(t, f.service)
	job, err := f.service.GetGeneration(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := f.service.Get(ctx, job.ProposalID)
	if err != nil || original.Status != "pending_review" {
		t.Fatal("candidate not evaluated", err)
	}
	old, err := readBehavior(ctx, f.db.DB(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"changed-evaluator", e.id} {
		next, err := New(ctx, f.registry, v)
		if err != nil {
			t.Fatal(err)
		}
		configureGeneration(t, next, g)
		evaluator := &fixtureBehaviorEvaluator{id: identity}
		configureBehavior(t, next, evaluator, behaviorSuite())
		f.service = next
		queued, err := next.recheckEnvironment(ctx, original.ID)
		if err != nil || !queued {
			t.Fatal("environment change not queued", err)
		}
		pending, err := next.Get(ctx, original.ID)
		if err != nil || pending.Status != "validating" || pending.CandidateVersion != original.CandidateVersion || pending.Base != original.Base {
			t.Fatal("recheck regenerated or adopted candidate", err)
		}
		if again, err := next.recheckEnvironment(ctx, original.ID); err != nil || again {
			t.Fatal("duplicate recheck queued", err)
		}
		generationRun(t, next)
		done, err := next.Get(ctx, original.ID)
		if err != nil || done.Status != "pending_review" || done.Evaluation.Behavior != "passed" || evaluator.calls.Load() != 2 {
			t.Fatal("new environment did not rerun comparison", done.Status, done.Error, err)
		}
		if g.calls.Load() != 1 || f.head(t) != original.Base {
			t.Fatal("environment change reran generation or changed installed head")
		}
	}
	var archived int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_behavior_result_history WHERE evaluation_id=?`, old.input.ID).Scan(&archived); err != nil || archived != 2 {
		t.Fatal("prior evaluation evidence lost", archived, err)
	}
	// Manual requests still cannot adopt an automatic proposal.
	if _, err := f.service.Approve(ctx, original.ID, harness.SkillReview{ExpectedStateVersion: original.StateVersion, Actor: "desktop"}); err == nil {
		t.Fatal("manual bypass accepted")
	}
}
