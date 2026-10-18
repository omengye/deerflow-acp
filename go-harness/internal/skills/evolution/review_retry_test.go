package evolution

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestReviewRetryIsBoundedImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	v := &classifiedReviewFixture{failure: &harness.SkillValidationFailure{Code: "review_response_invalid", Stage: "quality", DetailCode: "invalid_json", Retryable: true}}
	f := newFixture(t, v)
	g := &fixtureGenerator{id: "review-retry-fixture"}
	configureGeneration(t, f.service, g)
	configureBehavior(t, f.service, &fixtureBehaviorEvaluator{id: "retry-behavior"}, behaviorSuite())
	request := f.request(t, "failed-review", "Verify sources with a clear result.")
	request.Origin = "manual_agent"
	p, err := f.service.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		generationRun(t, f.service)
		if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, p.BehaviorJobID); err != nil {
			t.Fatal(err)
		}
	}
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "failed" || !CanRetryReview(p) || v.calls != 2 {
		t.Fatalf("%+v %v calls %d", p, err, v.calls)
	}
	if _, err = f.service.RetryReview(ctx, p.ID, p.StateVersion+1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale retry", err)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := f.service.RetryReview(ctx, p.ID, p.StateVersion)
			ids <- r.ID
			errs <- e
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id string
	for next := range ids {
		if id != "" && id != next {
			t.Fatal("duplicate successors")
		}
		id = next
	}
	next, err := f.service.Get(ctx, id)
	if err != nil || next.ID == p.ID || next.ReviewRetryOf != p.ID || next.CandidateVersion != p.CandidateVersion || next.Base != p.Base || next.Origin != p.Origin || next.Status != "validating" {
		t.Fatalf("%+v %v", next, err)
	}
	old, _ := f.service.Get(ctx, p.ID)
	if old.Status != p.Status || old.StateVersion != p.StateVersion || f.head(t) != p.Base || g.calls.Load() != 0 {
		t.Fatal("retry changed original/live content")
	}
	// Rebuild service bindings against the same durable records, as after restart.
	reopened, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	configureGeneration(t, reopened, g)
	configureBehavior(t, reopened, &fixtureBehaviorEvaluator{id: "retry-behavior"}, behaviorSuite())
	again, err := reopened.RetryReview(ctx, p.ID, p.StateVersion)
	if err != nil || again.ID != next.ID {
		t.Fatal("retry lost across service restart", err)
	}
	for i := 0; i < 2; i++ {
		generationRun(t, reopened)
		_, _ = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, next.BehaviorJobID)
	}
	next, _ = reopened.Get(ctx, next.ID)
	if next.Status != "failed" || CanRetryReview(next) {
		t.Fatal("retry chain unbounded", next.Status)
	}
	if _, err = reopened.RetryReview(ctx, next.ID, next.StateVersion); err == nil {
		t.Fatal("successor retry accepted")
	}
}

func TestReviewRetryRejectsContentAndIntegrityFailures(t *testing.T) {
	p := Proposal{Status: "failed", Origin: "manual_agent"}
	for _, f := range []*harness.SkillValidationFailure{{Code: "security_rejected"}, {Code: "quality_rejected"}, {Code: "review_refused"}, {Code: "review_response_incomplete"}, {Code: "review_response_incomplete", DetailCode: "unexpected_finish_reason"}, {Code: "review_response_invalid", DetailCode: "version_mismatch"}, {Code: "review_response_invalid", DetailCode: "duplicate_field"}, {Code: "review_response_invalid", DetailCode: "unexpected_tool_calls"}} {
		p.ValidationFailure = f
		if CanRetryReview(p) {
			t.Fatal("unsafe retry", f)
		}
	}
	p.ValidationFailure = &harness.SkillValidationFailure{Code: "review_response_incomplete", DetailCode: "output_truncated"}
	if !CanRetryReview(p) {
		t.Fatal("known output truncation cannot be explicitly retried")
	}
}

func TestScriptPackageNeverAutomaticallyPublishesEvenWithAcknowledgment(t *testing.T) {
	f := newFixture(t, nil)
	request := f.request(t, "python-auto", "Use the packaged helper.")
	request.Files["run.py"] = []byte("print('sample')")
	p := f.submit(t, request)
	r := review(p)
	r.AcknowledgeUnverifiedScripts = true
	ctx := context.WithValue(context.Background(), autoPatchContextKey{}, true)
	if _, err := f.service.Approve(ctx, p.ID, r); !errors.Is(err, errAutoPatchReview) {
		t.Fatal("automatic scripts admitted", err)
	}
	saved, _ := f.service.Get(context.Background(), p.ID)
	if saved.Approval != nil || saved.Status != "pending_review" {
		t.Fatal("automatic approval recorded")
	}
}
