package evolution

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func submittedFixture(t *testing.T, withSuite bool) (*fixture, *fixtureGenerator, *fixtureGenerationValidator, *fixtureBehaviorEvaluator) {
	t.Helper()
	f, g, v := generationFixture(t)
	e := &fixtureBehaviorEvaluator{id: "submitted-behavior-v1", result: func(in harness.SkillBehaviorInput) (bool, error) {
		return in.Variant != "base" || in.Case.Kind != "targeted", nil
	}}
	options := harness.SkillBehaviorOptions{}
	if withSuite {
		suite := behaviorSuite()
		regression := suite.Cases[0]
		regression.ID, regression.Kind, regression.Holdout = "holdout", "regression", true
		suite.Cases = append(suite.Cases, regression)
		options.Suites = []harness.SkillBehaviorSuite{suite}
	}
	if err := f.service.ConfigureBehavior(e, options); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 40, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
		t.Fatal(err)
	}
	return f, g, v, e
}

func TestSubmittedCandidateUsesAutomaticPipeline(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := submittedFixture(t, true)
	base := f.head(t)
	r := f.request(t, "agent-candidate", "Verify sources before summarizing")
	p, err := f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace)
	if err != nil || p.Origin != "automatic" || p.Status != "validating" || p.BehaviorJobID == "" {
		t.Fatalf("submission did not queue automatic validation: %+v %v", p, err)
	}
	if g.calls.Load() != 0 || v.calls.Load() != 0 || e.calls.Load() != 0 || f.head(t) != base {
		t.Fatal("submission generated, validated, or published synchronously")
	}
	again, err := f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace)
	if err != nil || again.ID != p.ID || again.BehaviorJobID != p.BehaviorJobID {
		t.Fatal("replay did not preserve exact candidate and job", again, err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("manual approval remains available", err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "pending_review" || p.Evaluation.Behavior != "passed" || g.calls.Load() != 0 || v.calls.Load() != 1 || e.calls.Load() != 4 {
		t.Fatalf("submitted package did not use full independent validation: %+v %v calls=%d/%d/%d", p, err, g.calls.Load(), v.calls.Load(), e.calls.Load())
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "published" || p.ReviewedBy != "auto_patch" {
		t.Fatal("validated candidate was not automatically adopted", p, err)
	}
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Status != "observing" {
		t.Fatal("automatic observation was not exposed", progress, err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
		t.Fatal("publication became a human approval bypass")
	}
}

func TestSubmittedCandidateWaitsForEvidenceAndCannotDelete(t *testing.T) {
	ctx := context.Background()
	f, g, _, _ := submittedFixture(t, false)
	r := f.request(t, "needs-evidence", "Verify sources before summarizing")
	p, err := f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.RunGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Status != "blocked_evidence" || g.calls.Load() != 0 {
		t.Fatal("missing evidence was not preserved", progress, err)
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	if f.head(t) != r.Base {
		t.Fatal("unvalidated candidate changed the installed head")
	}
	r.Action, r.Files, r.RequestKey = "delete", nil, "cannot-delete"
	if _, err = f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("automatic candidate deletion accepted", err)
	}
}

func TestSubmittedCandidateScopeAndConfigurationFailClosed(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := submittedFixture(t, true)
	r := f.request(t, "wrong-workspace", "Verify the original sources")
	if _, err := f.service.SubmitAutomatic(ctx, r, t.TempDir()); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("candidate escaped its source workspace", err)
	}
	page, err := f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Proposals) != 0 {
		t.Fatal("rejected enqueue left an orphan proposal", page, err)
	}
	f.service.autoPatch = nil
	if _, err = f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("missing automatic pipeline fell back to manual review", err)
	}
}

func TestSubmittedCandidateBehaviorRegressionCannotBeApproved(t *testing.T) {
	ctx := context.Background()
	f, g, _, e := submittedFixture(t, true)
	e.result = func(in harness.SkillBehaviorInput) (bool, error) {
		return in.Variant == "base" || in.Case.Kind != "regression", nil
	}
	r := f.request(t, "regressed-candidate", "Verify the original sources")
	p, err := f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "failed" || p.Evaluation.Behavior != "failed" || g.calls.Load() != 0 {
		t.Fatal("behavior regression did not reject the supplied candidate", p, err)
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil || f.head(t) != r.Base {
		t.Fatal("rejected candidate bypassed automatic admission", err)
	}
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Status != "rejected" {
		t.Fatal("rejection not exposed", progress, err)
	}
}

func TestSubmittedCandidateScriptsWaitForIsolation(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // No Docker executable: runtime readiness is deterministic.
	ctx := context.Background()
	f, g, _, _ := submittedFixture(t, true)
	r := f.request(t, "script-candidate", "Run scripts/check.py before summarizing")
	r.Files["scripts/check.py"] = []byte("print('verified')\n")
	p, err := f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Status != "blocked_runtime" || progress.JobState != "waiting" || g.calls.Load() != 0 {
		t.Fatal("missing isolation did not preserve a waiting candidate", progress, err)
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
		t.Fatal("manual approval bypassed script isolation")
	}
	if f.head(t) != r.Base {
		t.Fatal("unexecuted script candidate changed the installed head")
	}
}

func TestSubmittedCandidateEnqueueFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := submittedFixture(t, true)
	_, err := f.db.DB().ExecContext(ctx, `CREATE TRIGGER fail_candidate_job BEFORE INSERT ON df_skill_generation_inputs BEGIN SELECT RAISE(ABORT, 'fixture-job-write'); END`)
	if err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "atomic-candidate", "Verify the original sources")
	if _, err = f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace); err == nil {
		t.Fatal("candidate submission hid queue storage failure")
	}
	page, err := f.service.QueryProposals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Proposals) != 0 {
		t.Fatal("queue failure left an orphan candidate", page, err)
	}
	jobs, err := f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(jobs.Jobs) != 0 || f.head(t) != r.Base {
		t.Fatal("queue failure left a job or changed the installed skill", jobs, err)
	}
}

func TestSubmittedCandidateGlobalSourcePinsOriginatingWorkspace(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := submittedFixture(t, true)
	workspace := f.cfg.Sources[0].Workspace
	if err := f.registry.Close(); err != nil {
		t.Fatal(err)
	}
	f.cfg.Sources[0].Scope, f.cfg.Sources[0].Workspace = harness.SkillScopeGlobal, ""
	var err error
	f.registry, err = skills.NewRegistry(ctx, f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	configureGeneration(t, f.service, g)
	if err = f.service.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{behaviorSuite()}}); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 40, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.SubmitAutomatic(ctx, f.request(t, "global-candidate", "Verify the original sources"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.withProposal(ctx, p.ID, func(tx *skills.PublicationTx, p *Proposal) error {
		bound, err := f.service.automaticWorkspace(ctx, tx.SQL(), *p)
		if err != nil || bound != workspace {
			t.Fatalf("global source lost originating workspace: %q %v", bound, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
