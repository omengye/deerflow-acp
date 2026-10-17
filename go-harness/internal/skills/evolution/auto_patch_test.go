package evolution

import (
	"context"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestAutoPatchPublicationObservationAndRollback(t *testing.T) {
	ctx := context.Background()
	f := newCollectorFixture(t)
	initial := f.submit(t, f.request(t, "adopt-reviewed-skill", "Reviewed original instructions"))
	if _, err := f.service.Approve(ctx, initial.ID, review(initial)); err != nil {
		t.Fatal(err)
	}
	v := &fixtureGenerationValidator{id: "review-fixture-v1"}
	var err error
	f.service, err = New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	g := &fixtureGenerator{id: "generation-fixture-v1"}
	configureGeneration(t, f.service, g)
	suite := behaviorSuite()
	regression := suite.Cases[0]
	regression.ID = "existing-behavior"
	regression.Kind = "regression"
	suite.Cases = append(suite.Cases, regression)
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	configureBehavior(t, f.service, e, suite)
	options := harness.SkillAutoPatchOptions{MaxChangedLines: 40, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
	if err = f.service.ConfigureAutoPatch(options); err != nil {
		t.Fatal(err)
	}
	status, err := f.service.AutoPatchStatus(ctx)
	if err != nil || status.Readiness == nil || status.Readiness.State != "ready" || status.Readiness.EligibleTargets != 1 || !status.Readiness.NewSkillsRequireReview {
		t.Fatalf("host readiness: %+v %v", status, err)
	}
	base := f.head(t)
	unchanged := queueGeneration(t, f.fixture, "auto-patch-no-improvement")
	generationRun(t, f.service)
	unchangedJob, err := f.service.GetGeneration(ctx, unchanged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	unimproved, err := f.service.Get(ctx, unchangedJob.ProposalID)
	if err != nil || unimproved.Status != "pending_review" {
		t.Fatal("passing variants mistaken for improvement", unimproved, err)
	}
	attention := true
	pageBefore, err := f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 1, Status: "pending_review", NeedsAttention: &attention})
	if err != nil || len(pageBefore.Proposals) != 1 || pageBefore.Proposals[0].Disposition.Reason != "improvement_or_metadata_requires_review" {
		t.Fatal(pageBefore, err)
	}
	if _, err = f.service.Reject(ctx, unimproved.ID, review(unimproved)); err != nil {
		t.Fatal(err)
	}
	e.result = func(in harness.SkillBehaviorInput) (bool, error) {
		return in.Variant != "base" || in.Case.Kind != "targeted", nil
	}
	job := queueGeneration(t, f.fixture, "auto-patch")
	generationRun(t, f.service)
	generated, err := f.service.GetGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	proposal, err := f.service.Get(ctx, generated.ProposalID)
	if err != nil || proposal.Status != "published" || proposal.ReviewedBy != "auto_patch" {
		t.Fatalf("not automatically published: %+v %v", proposal, err)
	}
	if proposal.Approval == nil || proposal.Approval.Review.Actor != "auto_patch" || proposal.Approval.CandidateVersion != proposal.CandidateVersion {
		t.Fatal("automatic publication must retain policy approval without human review")
	}
	retried, err := f.service.RetryPublication(ctx, proposal.ID, proposal.Approval.Review.ExpectedStateVersion)
	if err != nil || retried.ReviewedBy != "auto_patch" || *retried.Approval != *proposal.Approval || retried.PublishedRevision != proposal.PublishedRevision {
		t.Fatal("operator retry replaced the automatic decision", err)
	}
	o, err := readAutoPatchObservation(ctx, f.db.DB(), proposal.ID)
	if err != nil || o.Status != "observing" || o.BaselineRevision != 2 || f.head(t) != o.Head {
		t.Fatalf("publication and observation not atomic: %+v %v", o, err)
	}
	// New conversations observe the new immutable version.
	snapshot, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.WithReadObserver(func(_ context.Context, r skills.ReadObservation) error { f.read = r; return nil }).Get(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	addFeedback := func(kind, key string) harness.SkillSignalEvidence {
		run := f.run(t, "", 1, "completed", false)
		f.drain(t, 128)
		evidence := feedbackEvidence(t, f, run)
		r := feedbackRequest(evidence, key)
		r.Kind = kind
		mustSubmitFeedback(t, f.service, r)
		return evidence
	}
	addFeedback("network", "external-failure")
	first := addFeedback("quality_failure", "first-quality-failure")
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), proposal.ID)
	if err != nil || o.Failures != 1 || o.Ignored != 1 || o.Status != "observing" {
		t.Fatalf("external failure affected quality: %+v %v", o, err)
	}
	// Latest explicit withdrawal removes the old vote before a decision.
	request := feedbackRequest(first, "withdraw-first")
	request.ExpectedFeedbackVersion = 1
	request.Kind = "withdrawn"
	request.ReadIDs = nil
	request.Summary = ""
	mustSubmitFeedback(t, f.service, request)
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), proposal.ID)
	if err != nil || o.Failures != 0 {
		t.Fatalf("withdrawn feedback counted: %+v %v", o, err)
	}
	addFeedback("quality_failure", "new-quality-one")
	addFeedback("helpful", "confirmed-success-breaks-streak")
	addFeedback("quality_failure", "new-quality-two")
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), proposal.ID)
	if err != nil || o.Failures != 2 || o.ConsecutiveFailures != 1 || o.Status != "observing" {
		t.Fatalf("success did not reset failure streak: %+v %v", o, err)
	}
	addFeedback("quality_failure", "new-quality-three")
	// Reopen the service before monitoring; neither grants nor volatile counters
	// decide publication identity, outcome counts or rollback revision.
	if err = f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	configureGeneration(t, f.service, g)
	configureBehavior(t, f.service, e, suite)
	// Disabling new automatic publication must not disable non-paid safety
	// observation of an already adopted version.
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), proposal.ID)
	if err != nil || o.Status != "rolled_back" || o.Failures != 3 || o.ConsecutiveFailures != 2 || o.RollbackRevision != 4 || f.head(t).Version != base.Version {
		t.Fatalf("confirmed regressions not rolled back: %+v %v", o, err)
	}
	before := f.head(t)
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.head(t) != before {
		t.Fatal("monitor replay produced another rollback")
	}
	page, err := f.service.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Observations) != 1 || page.Observations[0].RollbackRevision != 4 {
		t.Fatalf("monitor history=%+v %v", page, err)
	}
	// Terminal observations remain immutable even after the observation window.
	if err = f.service.ConfigureAutoPatch(options); err != nil {
		t.Fatal(err)
	}
	f.service.autoPatch.now = func() time.Time { return time.Now().Add(73 * time.Hour) }
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.head(t) != before {
		t.Fatal("terminal observation mutated later head")
	}
}
