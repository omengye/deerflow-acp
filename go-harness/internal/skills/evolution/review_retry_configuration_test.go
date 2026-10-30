package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type configurationRetryValidator struct {
	id      string
	failure *harness.SkillValidationFailure
	calls   int
}

func (v *configurationRetryValidator) Identity() string { return v.id }
func (v *configurationRetryValidator) Validate(ctx context.Context, input ValidationInput) (Report, error) {
	v.calls++
	report := Report{ValidatorVersion: v.id, BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
	if err := generationCharge(ctx, "security"); err != nil {
		return report, err
	}
	if v.failure != nil {
		return report, v.failure
	}
	report.Security = "passed"
	report.ModelChecks = []harness.SkillModelCheck{{Stage: "security", Passed: true}}
	if err := generationCharge(ctx, "quality"); err != nil {
		return report, err
	}
	report.ModelChecks = append(report.ModelChecks, harness.SkillModelCheck{Stage: "quality", Passed: true})
	return report, nil
}

func TestReviewRetryAfterConfigurationChangeRunsFullReviewOnce(t *testing.T) {
	for _, outcome := range []string{"corrected", "still-missing"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			old := &configurationRetryValidator{id: "review-config-old", failure: &harness.SkillValidationFailure{Code: "review_request_failed", Stage: "security"}}
			f := newFixture(t, old)
			generator := &fixtureGenerator{id: "config-retry-generator"}
			configureGeneration(t, f.service, generator)
			configureBehavior(t, f.service, &fixtureBehaviorEvaluator{id: "config-retry-behavior"}, behaviorSuite())
			request := f.request(t, "configuration-retry", "Verify every source before summarizing.")
			request.Origin, request.Author = "manual_agent", "model/original-session"
			original, err := f.service.Submit(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			generationRun(t, f.service)
			original, err = f.service.Get(ctx, original.ID)
			if err != nil || original.Status != "failed" {
				t.Fatalf("legacy fixture: %+v %v", original, err)
			}
			failed, err := f.service.RetryReview(ctx, original.ID, original.StateVersion)
			if err != nil {
				t.Fatal(err)
			}
			// The same configuration now records the precise provider failure.
			old.failure = &harness.SkillValidationFailure{Code: "review_request_failed", Stage: "security", DetailCode: "http_404"}
			generationRun(t, f.service)
			failed, err = f.service.Get(ctx, failed.ID)
			if err != nil || failed.Status != "failed" || failed.ReviewRetryOf != original.ID || f.service.ReviewRetryAvailable(failed) || CanRetryReview(failed) {
				t.Fatalf("same configuration must stop the chain: %+v %v", failed, err)
			}
			if _, err = f.service.RetryReview(ctx, failed.ID, failed.StateVersion); !errors.Is(err, harness.ErrSkillConflict) {
				t.Fatal("same configuration accepted", err)
			}
			fixed := &configurationRetryValidator{id: "review-config-corrected"}
			if outcome == "still-missing" {
				fixed.failure = old.failure
			}
			service, err := New(ctx, f.registry, fixed)
			if err != nil {
				t.Fatal(err)
			}
			configureGeneration(t, service, generator)
			behavior := &fixtureBehaviorEvaluator{id: "config-retry-behavior"}
			configureBehavior(t, service, behavior, behaviorSuite())
			if !service.ReviewRetryAvailable(failed) || CanRetryReview(failed) {
				t.Fatal("changed host configuration was not distinguished from ordinary retry")
			}
			if _, err = service.RetryReview(ctx, failed.ID, failed.StateVersion+1); !errors.Is(err, harness.ErrSkillConflict) {
				t.Fatal("stale configuration retry accepted", err)
			}
			next, err := service.RetryReview(ctx, failed.ID, failed.StateVersion)
			if err != nil || next.ID == failed.ID || next.ReviewRetryOf != failed.ID || next.Author != failed.Author || next.Target != failed.Target || next.Base != failed.Base || next.CandidateVersion != failed.CandidateVersion || next.Reason != failed.Reason || next.Approval != nil || next.Status != "validating" {
				t.Fatalf("changed-configuration successor: %+v %v", next, err)
			}
			duplicate, err := service.RetryReview(ctx, failed.ID, failed.StateVersion)
			if err != nil || duplicate.ID != next.ID {
				t.Fatal("configuration retry was not idempotent", err)
			}
			generationRun(t, service)
			next, err = service.Get(ctx, next.ID)
			if err != nil || next.Approval != nil || service.ReviewRetryAvailable(next) || fixed.calls != 1 || generator.calls.Load() != 0 || f.head(t) != failed.Base {
				t.Fatalf("review changed approval/content or allowed another retry: %+v %v", next, err)
			}
			if outcome == "corrected" {
				receipts, e := service.BehaviorReceipts(ctx, next.ID, 0, 10)
				if e != nil || next.Status != "pending_review" || next.Evaluation.ValidatorVersion != fixed.id || next.Evaluation.Security != "passed" || next.Evaluation.Behavior != "passed" || len(receipts) != 4 || behavior.calls.Load() != 2 {
					t.Fatalf("full security/quality/behavior review did not run: %+v %+v %v", next, receipts, e)
				}
				for i, stage := range []string{"security", "quality", "behavior", "behavior"} {
					if receipts[i].Stage != stage {
						t.Fatalf("review stage omitted: %+v", receipts)
					}
				}
			} else if next.Status != "failed" || next.ValidationFailure.DetailCode != "http_404" || behavior.calls.Load() != 0 {
				t.Fatalf("failed corrected configuration did not stop: %+v", next)
			}
			unchanged, err := service.Get(ctx, failed.ID)
			if err != nil || encodedHash(unchanged) != encodedHash(failed) {
				t.Fatal("configuration retry rewrote the failed evidence", err)
			}
		})
	}
}

func TestReviewRetryConfigurationChangeRejectsUnprovenOrDeniedEvidence(t *testing.T) {
	for _, mode := range []string{"changed", "same", "no-old-identity", "no-current-identity", "unknown", "authentication", "other-stage", "content-deny", "quality-deny", "behavior-deny", "approval", "automatic", "invalid-report"} {
		t.Run(mode, func(t *testing.T) {
			validator := &configurationRetryValidator{id: "new"}
			service := &Service{validator: validator}
			p := Proposal{Status: "failed", Origin: "manual_agent", ReviewRetryOf: "previous", CandidateVersion: strings.Repeat("a", 64),
				Evaluation:        Report{ValidatorVersion: "old", CandidateVersion: strings.Repeat("a", 64), Security: "unavailable", Behavior: "not_run"},
				ValidationFailure: &harness.SkillValidationFailure{Code: "review_request_failed", Stage: "security", DetailCode: "http_404"}}
			switch mode {
			case "same":
				validator.id = "old"
			case "no-old-identity":
				p.Evaluation.ValidatorVersion = ""
			case "no-current-identity":
				validator.id = ""
			case "unknown":
				p.ValidationFailure.DetailCode = "cause_unavailable"
			case "authentication":
				p.ValidationFailure.DetailCode = "http_401"
			case "other-stage":
				p.ValidationFailure.Stage = "validation"
			case "content-deny":
				p.Evaluation.Security = "failed"
			case "quality-deny":
				p.Evaluation.ModelChecks = []harness.SkillModelCheck{{Stage: "quality", Passed: false}}
			case "behavior-deny":
				p.Evaluation.Behavior = "failed"
			case "approval":
				p.Approval = &harness.SkillApproval{}
			case "automatic":
				p.Origin = "generated"
			case "invalid-report":
				p.Evaluation.CandidateVersion = strings.Repeat("b", 64)
			}
			if got := service.reviewRetryAfterConfigurationChange(p); got != (mode == "changed") {
				t.Fatalf("configuration retry eligibility=%t for %s", got, mode)
			}
			if CanRetryReview(p) || service.ReviewRetryAvailable(p) {
				t.Fatal("static retry or missing native maintenance bypassed configuration guard")
			}
		})
	}
}
