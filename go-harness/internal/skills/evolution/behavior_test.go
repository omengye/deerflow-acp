package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type fixtureBehaviorEvaluator struct {
	id     string
	calls  atomic.Int32
	before func(context.Context, harness.SkillBehaviorInput)
	after  func(context.Context, harness.SkillBehaviorInput)
	result func(harness.SkillBehaviorInput) (bool, error)
}

func (e *fixtureBehaviorEvaluator) Identity() string { return e.id }
func (e *fixtureBehaviorEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	if e.before != nil {
		e.before(ctx, in)
	}
	if err := generationCharge(ctx, "behavior"); err != nil {
		return harness.SkillBehaviorResult{}, err
	}
	e.calls.Add(1)
	if e.after != nil {
		e.after(ctx, in)
	}
	passed := true
	if e.result != nil {
		var err error
		passed, err = e.result(in)
		if err != nil {
			return harness.SkillBehaviorResult{}, err
		}
	}
	r := harness.SkillBehaviorResult{CaseID: in.CaseID, Variant: in.Variant, Version: in.Version, Repetition: in.Repetition, Passed: passed, StopReason: "completed", OutputDigest: hash([]byte(in.Variant)), TraceDigest: hash(nil), ModelCalls: 1, InputTokens: 10, OutputTokens: 5, ElapsedMillis: 1}
	if !passed {
		r.ErrorCode = "assertion_failed"
	}
	for _, a := range in.Case.Assertions {
		r.Checks = append(r.Checks, harness.SkillBehaviorCheck{ID: a.ID, Passed: passed, ActualDigest: hash(nil)})
	}
	return r, nil
}
func behaviorSuite() harness.SkillBehaviorSuite {
	return harness.SkillBehaviorSuite{Version: "skill-behavior-v1", ID: "research-verification", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{{ID: "verify", Kind: "targeted", Prompt: "Verify sources", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "output_contains", Expected: "verified"}}}}}
}
func configureBehavior(t *testing.T, s *Service, e *fixtureBehaviorEvaluator, suite harness.SkillBehaviorSuite) {
	t.Helper()
	if err := s.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}); err != nil {
		t.Fatal(err)
	}
}
func behaviorFixture(t *testing.T) (*fixture, *fixtureGenerator, *fixtureGenerationValidator, *fixtureBehaviorEvaluator) {
	t.Helper()
	f, g, v := generationFixture(t)
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	configureBehavior(t, f.service, e, behaviorSuite())
	return f, g, v, e
}
func queueManualBehavior(t *testing.T, f *fixture, key string) Proposal {
	t.Helper()
	p, err := f.service.Submit(context.Background(), f.request(t, key, "Verify the source before summarizing"))
	if err != nil || p.Status != "validating" || p.BehaviorJobID == "" {
		t.Fatalf("queue manual %+v %v", p, err)
	}
	return p
}

func TestBehaviorManualQueuedBudgetedReviewedAndPublishedSeparately(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := behaviorFixture(t)
	base := f.head(t)
	r := f.request(t, "manual", "Verify the source before summarizing")
	p, err := f.service.Submit(ctx, r)
	if err != nil || p.Status != "validating" || p.BehaviorJobID == "" || g.calls.Load() != 0 || v.calls.Load() != 0 || e.calls.Load() != 0 {
		t.Fatalf("submission ran maintenance %+v %v", p, err)
	}
	again, err := f.service.Submit(ctx, r)
	if err != nil || again.ID != p.ID || again.BehaviorJobID != p.BehaviorJobID {
		t.Fatalf("idempotent enqueue %+v %v", again, err)
	}
	if _, err = f.service.Validate(ctx, p.ID, p.StateVersion); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("unbudgeted review admitted %v", err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "pending_review" || p.Evaluation.Behavior != "passed" || p.Evaluation.BehaviorReport == nil || p.Evaluation.IsolatedScripts {
		t.Fatalf("review %+v %v", p, err)
	}
	view, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil || view.State != "completed" || !view.ContentReviewed || len(view.Results) != 2 || view.Report == nil || view.Report.TargetedCases != 1 || view.Report.RegressionCases != 0 {
		t.Fatalf("view %+v %v", view, err)
	}
	receipts, err := f.service.BehaviorReceipts(ctx, p.ID, 0, 100)
	if err != nil || len(receipts) != 4 {
		t.Fatalf("receipts %+v %v", receipts, err)
	}
	for i, stage := range []string{"security", "quality", "behavior", "behavior"} {
		if receipts[i].Stage != stage || receipts[i].JobID != p.BehaviorJobID || receipts[i].ChargedTokens != 15 {
			t.Fatal(receipts)
		}
	}
	if g.calls.Load() != 0 || v.calls.Load() != 1 || e.calls.Load() != 2 || f.head(t) != base {
		t.Fatal("manual producer generated or published")
	}
	if jobs, err := f.service.QueryGeneration(ctx, harness.SkillQuery{Limit: 10}); err != nil || len(jobs.Jobs) != 0 {
		t.Fatalf("manual job masquerades as generation %+v %v", jobs, err)
	}
	// Native evidence remains manually reviewable after maintenance is disabled.
	disabled, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = disabled.Approve(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	if f.head(t).Version != p.CandidateVersion {
		t.Fatal("approval did not publish")
	}
}

func TestBehaviorGeneratedCandidateUsesOriginalJob(t *testing.T) {
	ctx := context.Background()
	f, g, v, e := behaviorFixture(t)
	job := queueGeneration(t, f, "native-generation")
	generationRun(t, f.service)
	got, err := f.service.GetGeneration(ctx, job.ID)
	if err != nil || got.State != "completed" {
		t.Fatalf("generation %+v %v", got, err)
	}
	p, err := f.service.Get(ctx, got.ProposalID)
	if err != nil || p.Status != "pending_review" || p.BehaviorJobID != "" {
		t.Fatalf("proposal %+v %v", p, err)
	}
	view, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil || view.JobID != job.ID || len(view.Results) != 2 {
		t.Fatalf("behavior %+v %v", view, err)
	}
	var jobs int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("extra jobs %d %v", jobs, err)
	}
	r, err := f.service.BehaviorReceipts(ctx, p.ID, 0, 100)
	if err != nil || len(r) != 5 || g.calls.Load() != 1 || v.calls.Load() != 1 || e.calls.Load() != 2 {
		t.Fatalf("accounting %+v %v", r, err)
	}
}

func TestBehaviorResumesCompletedCasesAndContentAcrossRestart(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	suite := behaviorSuite()
	suite.Cases[0].Repetitions = 2
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	var failed atomic.Bool
	e.result = func(in harness.SkillBehaviorInput) (bool, error) {
		if in.Variant == "candidate" && !failed.Swap(true) {
			return false, harness.ErrSkillValidation
		}
		return true, nil
	}
	configureBehavior(t, f.service, e, suite)
	p := queueManualBehavior(t, f, "resume")
	claim, ok, err := f.service.generation.store.Claim(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	work := context.WithValue(skillmaintenance.WithBroker(ctx, f.service.generation.store.Broker(claim)), generationContextKey{}, p.BehaviorJobID)
	if _, err = f.service.Validate(work, p.ID, p.StateVersion); !errors.Is(err, harness.ErrSkillValidation) {
		t.Fatalf("expected interrupted case %v", err)
	}
	before, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil || len(before.Results) != 1 || before.Report != nil || !before.ContentReviewed {
		t.Fatalf("partial checkpoint %+v %v", before, err)
	}
	if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET lease_ns=0 WHERE id=?`, p.BehaviorJobID); err != nil {
		t.Fatal(err)
	}
	next, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	configureGeneration(t, next, g)
	configureBehavior(t, next, e, suite)
	generationRun(t, next)
	view, err := next.BehaviorEvaluation(ctx, p.ID)
	if err != nil || view.State != "completed" || view.Attempts != 2 || len(view.Results) != 4 || view.Report == nil || len(view.Report.Results) != 2 || view.Report.TargetedCases != 1 || view.Deadline != before.Deadline {
		t.Fatalf("recovery %+v %v", view, err)
	}
	if v.calls.Load() != 1 || e.calls.Load() != 5 || g.calls.Load() != 0 {
		t.Fatalf("replayed completed work content=%d behavior=%d", v.calls.Load(), e.calls.Load())
	}
	r, err := next.BehaviorReceipts(ctx, p.ID, 0, 100)
	if err != nil || len(r) != 7 || r[3].Attempt != 1 || r[4].Attempt != 2 {
		t.Fatalf("retry charges %+v %v", r, err)
	}
	if _, err = f.service.Validate(work, p.ID, 1); err == nil {
		t.Fatal("expired claim committed")
	}
}

func TestBehaviorCreateAndDeleteExecuteEmptyControls(t *testing.T) {
	for _, action := range []string{"create", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			f, g, v := generationFixture(t)
			suite := behaviorSuite()
			e := &fixtureBehaviorEvaluator{id: "behavior-empty-v1"}
			emptyCalls := 0
			e.result = func(in harness.SkillBehaviorInput) (bool, error) {
				empty := in.Variant == "base" && action == "create" || in.Variant == "candidate" && action == "delete"
				if empty {
					emptyCalls++
					if in.Version != "" || len(in.Files) != 0 {
						t.Fatalf("not an empty control %+v", in)
					}
				} else if in.Version == "" || len(in.Files) == 0 {
					t.Fatal("existing variant lost files")
				}
				return true, nil
			}
			r := Request{SourceID: "project", Name: "research", RequestKey: action, Action: action, Reason: "Test no-skill control", Base: f.head(t)}
			if action == "create" {
				suite.Name = "new-skill"
				r.Name = "new-skill"
				r.Base = harness.SkillHead{}
				r.Files = map[string][]byte{"SKILL.md": []byte("---\nname: new-skill\ndescription: A newly authored skill\n---\nVerify sources\n")}
			}
			configureBehavior(t, f.service, e, suite)
			p, err := f.service.Submit(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			generationRun(t, f.service)
			p, err = f.service.Get(ctx, p.ID)
			if err != nil || p.Status != "pending_review" || p.Evaluation.BehaviorReport == nil || emptyCalls != 1 || e.calls.Load() != 2 || g.calls.Load() != 0 || v.calls.Load() != 1 {
				t.Fatalf("empty control %+v %v", p, err)
			}
			if action == "create" && p.Evaluation.BehaviorReport.BaseVersion != "" || action == "delete" && p.Evaluation.BehaviorReport.CandidateVersion != "" {
				t.Fatal("empty control version mismatch")
			}
			if _, err = f.service.Approve(ctx, p.ID, review(p)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBehaviorNoMatchingSuiteRequiresManualReviewWithoutBehaviorClaims(t *testing.T) {
	ctx := context.Background()
	f, _, _ := generationFixture(t)
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	suite := behaviorSuite()
	suite.Name = "another-skill"
	configureBehavior(t, f.service, e, suite)
	p := queueManualBehavior(t, f, "unmatched")
	generationRun(t, f.service)
	p, err := f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "pending_review" || p.Evaluation.Behavior != "not_run" || p.Evaluation.BehaviorReport != nil || e.calls.Load() != 0 || !strings.Contains(strings.Join(p.Evaluation.Evidence, " "), "no trusted host suite") {
		t.Fatalf("no suite %+v %v", p, err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
}

func TestBehaviorComparisonsRejectRegressionsAndFailedTargetedCases(t *testing.T) {
	for _, mode := range []string{"targeted-failed", "regressed", "improved"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, _, _ := generationFixture(t)
			suite := behaviorSuite()
			reg := suite.Cases[0]
			reg.ID = "regression"
			reg.Kind = "regression"
			suite.Cases = append(suite.Cases, reg)
			e := &fixtureBehaviorEvaluator{id: "behavior-score-v1", result: func(in harness.SkillBehaviorInput) (bool, error) {
				switch mode {
				case "targeted-failed":
					return in.Variant == "base" || in.CaseID != "verify", nil
				case "regressed":
					return in.Variant == "base" || in.CaseID != "regression", nil
				default:
					return in.Variant == "candidate", nil
				}
			}}
			configureBehavior(t, f.service, e, suite)
			p := queueManualBehavior(t, f, mode)
			generationRun(t, f.service)
			p, err := f.service.Get(ctx, p.ID)
			if err != nil || p.Evaluation.BehaviorReport == nil {
				t.Fatalf("report %+v %v", p, err)
			}
			r := p.Evaluation.BehaviorReport
			if r.TargetedCases != 1 || r.RegressionCases != 1 || len(r.Results) != 2 {
				t.Fatal(r)
			}
			if mode == "improved" {
				if p.Status != "pending_review" || r.Improvements != 2 || r.Regressions != 0 {
					t.Fatal(r)
				}
			} else {
				if p.Status != "failed" || r.Regressions != 1 || r.State != "failed" {
					t.Fatal(r)
				}
				if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
					t.Fatal("failed behavior approved")
				}
			}
		})
	}
}

func TestBehaviorManualProposalAndJobInsertionAreAtomic(t *testing.T) {
	f, _, v, e := behaviorFixture(t)
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_behavior BEFORE INSERT ON df_skill_behavior_evaluations BEGIN SELECT RAISE(ABORT,'fixture-behavior-fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Submit(context.Background(), f.request(t, "atomic", "New instructions")); err == nil {
		t.Fatal("insert fault ignored")
	}
	for _, table := range []string{"df_skill_proposals", "df_skill_maintenance_jobs"} {
		var count int
		if err := f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan %s %d %v", table, count, err)
		}
	}
	if v.calls.Load() != 0 || e.calls.Load() != 0 {
		t.Fatal("failed insertion spent model calls")
	}
}

func TestBehaviorRejectFencesCallsAndLateResults(t *testing.T) {
	for _, origin := range []string{"manual", "generation"} {
		t.Run(origin, func(t *testing.T) {
			ctx := context.Background()
			f, _, _, e := behaviorFixture(t)
			var proposalID, jobID string
			if origin == "manual" {
				p := queueManualBehavior(t, f, "reject")
				proposalID, jobID = p.ID, p.BehaviorJobID
			} else {
				jobID = queueGeneration(t, f, "reject").ID
			}
			e.after = func(_ context.Context, _ harness.SkillBehaviorInput) {
				if proposalID == "" {
					j, err := f.service.GetGeneration(ctx, jobID)
					if err != nil {
						t.Fatal(err)
					}
					proposalID = j.ProposalID
				}
				p, err := f.service.Get(ctx, proposalID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.service.Reject(ctx, p.ID, review(p)); err != nil {
					t.Fatal(err)
				}
			}
			generationRun(t, f.service)
			view, err := f.service.BehaviorEvaluation(ctx, proposalID)
			if err != nil || view.State != "cancelled" || len(view.Results) != 0 || view.Report != nil || e.calls.Load() != 1 {
				t.Fatalf("late result %+v %v", view, err)
			}
			p, err := f.service.Get(ctx, proposalID)
			if err != nil || p.Status != "rejected" {
				t.Fatalf("reject %+v %v", p, err)
			}
			if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
				t.Fatal("rejected proposal approved")
			}
		})
	}
}

func TestBehaviorAdmissionFencesHeadSourceAndConfiguration(t *testing.T) {
	for _, mode := range []string{"head", "source", "reviewer", "evaluator", "suite"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, g, v, e := behaviorFixture(t)
			p := queueManualBehavior(t, f, mode)
			s := f.service
			want := "configuration_changed"
			switch mode {
			case "head":
				want = "base_changed"
				if _, err := f.registry.SetEnabledChecked(ctx, harness.SkillEnablement{Target: p.Target, Expected: p.Base, Enabled: false, RequestKey: "disable", Actor: "operator"}); err != nil {
					t.Fatal(err)
				}
			case "source":
				want = "source_unavailable"
				s.generation.allowed = map[string]harness.SkillTarget{}
			default:
				nextV := v
				if mode == "reviewer" {
					nextV = &fixtureGenerationValidator{id: "changed-reviewer"}
				}
				next, err := New(ctx, f.registry, nextV)
				if err != nil {
					t.Fatal(err)
				}
				configureGeneration(t, next, g)
				suite := behaviorSuite()
				nextE := e
				if mode == "evaluator" {
					nextE = &fixtureBehaviorEvaluator{id: "changed-evaluator"}
				}
				if mode == "suite" {
					suite.Cases[0].Assertions[0].Expected = "different oracle"
				}
				configureBehavior(t, next, nextE, suite)
				s = next
			}
			generationRun(t, s)
			view, err := s.BehaviorEvaluation(ctx, p.ID)
			if err != nil || view.State != "failed" || view.ErrorCode != want || len(view.Results) != 0 || view.Report != nil {
				t.Fatalf("admission %+v %v", view, err)
			}
			stored, err := s.Get(ctx, p.ID)
			if err != nil || stored.Status == "validating" || stored.Evaluation.Behavior != "unavailable" {
				t.Fatalf("terminal proposal %+v %v", stored, err)
			}
			r, err := s.BehaviorReceipts(ctx, p.ID, 0, 100)
			if err != nil || len(r) != 0 || e.calls.Load() != 0 {
				t.Fatalf("unexpected charges %+v %v", r, err)
			}
		})
	}
}

func TestBehaviorBudgetExhaustionPreservesPartialEvidenceWithoutFallback(t *testing.T) {
	ctx := context.Background()
	v := &fixtureGenerationValidator{id: "review-fixture-v1"}
	f := newFixture(t, v)
	g := &fixtureGenerator{id: "generation-fixture-v1"}
	if err := f.service.ConfigureGeneration(ctx, g, skillmaintenance.Policy{MaxDailyModelCalls: 3, MaxDailyTokens: 100000, MaxJobSeconds: 300, Workers: 1}, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	configureBehavior(t, f.service, e, behaviorSuite())
	p := queueManualBehavior(t, f, "budget")
	generationRun(t, f.service)
	view, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil || view.State != "failed" || view.ErrorCode != "budget_exhausted" || len(view.Results) != 1 || view.Report != nil || e.calls.Load() != 1 {
		t.Fatalf("budget %+v %v", view, err)
	}
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "failed" || p.Evaluation.Behavior != "unavailable" || p.Evaluation.BehaviorReport != nil {
		t.Fatalf("fallback %+v %v", p, err)
	}
}

func TestBehaviorApprovalReconstructsPersistedEvidence(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := behaviorFixture(t)
	p := queueManualBehavior(t, f, "proof")
	generationRun(t, f.service)
	p, err := f.service.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	value := view.Results[0]
	value.OutputDigest = hash([]byte("tampered"))
	data, _ := json.Marshal(value)
	if _, err = f.db.DB().Exec(`UPDATE df_skill_behavior_results SET payload=?,payload_sha=? WHERE evaluation_id=? AND case_id=? AND variant=? AND repetition=?`, data, hash(data), view.ID, value.CaseID, value.Variant, value.Repetition); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err == nil {
		t.Fatal("stored report hid modified case")
	}
	if _, err = f.service.BehaviorEvaluation(ctx, p.ID); err == nil {
		t.Fatal("query accepted modified case")
	}
}

func TestBehaviorLegacyValidatorCannotMintNativeProvenance(t *testing.T) {
	f := newFixture(t, validatorFunc(func(_ context.Context, in ValidationInput) (Report, error) {
		r := validReport(in)
		r.BehaviorReport = &harness.SkillBehaviorReport{State: "passed"}
		r.BehaviorDigest = encodedHash(*r.BehaviorReport)
		return r, nil
	}))
	p := f.submit(t, f.request(t, "legacy", "New instructions"))
	if p.Evaluation.Behavior != "passed" || p.Evaluation.BehaviorReport != nil || p.Evaluation.BehaviorDigest != "" {
		t.Fatalf("legacy claim %+v", p)
	}
	if _, err := f.service.BehaviorEvaluation(context.Background(), p.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.service.Approve(context.Background(), p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
}

func TestBehaviorNativeReviewDoesNotAuthorizeScriptExecution(t *testing.T) {
	ctx := context.Background()
	f, _, _, e := behaviorFixture(t)
	r := f.request(t, "scripts", "Run the [helper](helper.go)")
	r.Files["helper.go"] = []byte("package main\nfunc main(){}\n")
	p, err := f.service.Submit(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	p, err = f.service.Get(ctx, p.ID)
	if err != nil || p.Status != "failed" || p.Evaluation.IsolatedScripts || e.calls.Load() != 0 {
		t.Fatalf("script review %+v %v", p, err)
	}
}

func TestBehaviorEvaluationRequiresConfiguredGenerationAndTrustedSuite(t *testing.T) {
	f := newFixture(t, nil)
	e := &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
	if err := f.service.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{behaviorSuite()}}); err == nil {
		t.Fatal("behavior enabled without maintenance")
	}
	f, _, _ = generationFixture(t)
	suite := behaviorSuite()
	suite.SourceID = "ungranted"
	if err := f.service.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal(err)
	}
}

func TestBehaviorLateResultsAreFencedByHeadLeaseAndDiscoveryEvidence(t *testing.T) {
	for _, mode := range []string{"head", "lease", "discovery"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var f *fixture
			var e *fixtureBehaviorEvaluator
			var id string
			var alter func()
			if mode == "discovery" {
				collector, _, _ := discoveryFixture(t)
				f = collector.fixture
				e = &fixtureBehaviorEvaluator{id: "behavior-fixture-v1"}
				configureBehavior(t, f.service, e, behaviorSuite())
				first := discoveryCorrection(t, collector, "", "first")
				discoveryCorrection(t, collector, "", "second")
				discoverySteps(t, f.service, 2)
				id = discoveries(t, f.service)[0].GenerationID
				alter = func() { withdrawDiscoveryFeedback(t, f.service, first.SignalID) }
			} else {
				f, _, _, e = behaviorFixture(t)
				p := queueManualBehavior(t, f, mode)
				id = p.BehaviorJobID
				alter = func() {
					if mode == "head" {
						if _, err := f.registry.SetEnabledChecked(ctx, harness.SkillEnablement{Target: p.Target, Expected: p.Base, Enabled: false, RequestKey: "during-model", Actor: "operator"}); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET lease_ns=0 WHERE id=?`, id); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			var changed atomic.Bool
			e.after = func(context.Context, harness.SkillBehaviorInput) {
				if !changed.Swap(true) {
					alter()
				}
			}
			generationRun(t, f.service)
			var proposalID string
			if mode == "discovery" {
				j, err := f.service.GetGeneration(ctx, id)
				if err != nil || j.ErrorCode != "evidence_changed" {
					t.Fatalf("discovery fence %+v %v", j, err)
				}
				proposalID = j.ProposalID
			} else {
				if err := f.db.DB().QueryRow(`SELECT proposal_id FROM df_skill_behavior_evaluations WHERE job_id=?`, id).Scan(&proposalID); err != nil {
					t.Fatal(err)
				}
			}
			view, err := f.service.BehaviorEvaluation(ctx, proposalID)
			if err != nil || len(view.Results) != 0 || view.Report != nil || e.calls.Load() != 1 {
				t.Fatalf("late result persisted %+v %v", view, err)
			}
			if mode == "lease" {
				if view.State != "retry" {
					t.Fatalf("lease loss not recoverable %+v", view)
				}
				if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
				generationRun(t, f.service)
				again, err := f.service.BehaviorEvaluation(ctx, proposalID)
				if err != nil || again.State != "completed" || again.Deadline != view.Deadline || again.Attempts != 2 || len(again.Results) != 2 || e.calls.Load() != 3 {
					t.Fatalf("lease recovery %+v %v", again, err)
				}
			} else {
				p, err := f.service.Get(ctx, proposalID)
				if err != nil || p.Evaluation.Behavior != "unavailable" || p.Status == "pending_review" || view.State != "failed" {
					t.Fatalf("late failure fallback %+v %v", p, err)
				}
			}
		})
	}
}
