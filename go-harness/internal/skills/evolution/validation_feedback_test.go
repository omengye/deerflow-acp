package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestProposalValidationFailureExplainsStageAndSurvivesReplay(t *testing.T) {
	for _, mode := range []string{"quality-rejected", "review-unavailable", "quality-timeout"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			f := newFixture(t, validatorFunc(func(_ context.Context, in ValidationInput) (Report, error) {
				calls++
				r := validReport(in)
				r.Behavior = "not_run"
				if mode == "review-unavailable" {
					return r, errors.New("private-review-transport-secret")
				}
				if mode == "quality-timeout" {
					return r, &harness.SkillValidationFailure{Code: "review_timeout", Stage: "quality", Retryable: true}
				}
				r.ModelChecks = append(r.ModelChecks, harness.SkillModelCheck{Stage: "quality", Reason: "Missing consistency check."})
				return r, nil
			}))
			request := f.request(t, mode, "Candidate instructions")
			p, err := f.service.Submit(context.Background(), request)
			if err != nil || p.Status != "failed" {
				t.Fatalf("expected persisted failure: %+v %v", p, err)
			}
			want := "quality_rejected"
			if mode == "review-unavailable" {
				want = "validation_unavailable"
			} else if mode == "quality-timeout" {
				want = "review_timeout"
			}
			for range 2 {
				data, _ := json.Marshal(p)
				var saved struct {
					Failure struct {
						Code      string `json:"code"`
						Retryable bool   `json:"retryable"`
					} `json:"validation_failure"`
				}
				if err := json.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.Failure.Code != want || saved.Failure.Retryable != (mode == "quality-timeout") || strings.Contains(string(data), "private-review-transport-secret") || strings.Contains(p.Error, "unavailable or did not pass") {
					t.Fatalf("ambiguous/unsafe failure: %s", data)
				}
				p, err = f.service.Submit(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 || f.head(t) != request.Base {
				t.Fatal("feedback/replay reran review or changed publication")
			}
			reopened, err := New(context.Background(), f.registry, nil)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := reopened.Get(context.Background(), p.ID)
			if err != nil || !reflect.DeepEqual(restored.ValidationFailure, p.ValidationFailure) || restored.EvaluationDigest != p.EvaluationDigest {
				t.Fatalf("reopening lost classification or rewrote review evidence: %+v %v", restored, err)
			}
			if _, err = f.service.Approve(context.Background(), p.ID, review(p)); err == nil {
				t.Fatal("failed review became approvable")
			}
		})
	}
}

func TestGenerationReviewRetriesUseClassifiedFailure(t *testing.T) {
	for _, code := range []string{"review_response_invalid", "review_configuration_error", "review_temporarily_unavailable", "review_timeout"} {
		wantRetry := code == "review_temporarily_unavailable" || code == "review_timeout"
		gotCode, retry := generationFailure(&harness.SkillValidationFailure{Code: code, Stage: "quality", Retryable: wantRetry})
		if gotCode != "model_failed" || retry != wantRetry {
			t.Fatalf("failure %s changed retry policy: %s %t", code, gotCode, retry)
		}
	}
}

type classifiedReviewFixture struct {
	failure *harness.SkillValidationFailure
	calls   int
}

func (v *classifiedReviewFixture) Identity() string { return "classified-review-fixture" }

func (v *classifiedReviewFixture) Validate(ctx context.Context, in ValidationInput) (Report, error) {
	v.calls++
	r := validReport(in)
	r.ValidatorVersion, r.Behavior = v.Identity(), "not_run"
	r.ModelChecks = []harness.SkillModelCheck{{Stage: "security", Passed: true, Reason: "Scoped content."}}
	for _, stage := range []string{"security", "quality"} {
		if err := generationCharge(ctx, stage); err != nil {
			return r, err
		}
	}
	return r, v.failure
}

func TestMaintenanceReviewFeedbackPreservesCauseAndStopsAtAttemptLimit(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, transient := range []bool{false, true} {
			name := "generation"
			if manual {
				name = "manual-behavior"
			}
			if transient {
				name += "-transient"
			}
			t.Run(name, func(t *testing.T) {
				code, attempts := "review_response_invalid", 1
				if transient {
					code, attempts = "review_temporarily_unavailable", 3
				}
				v := &classifiedReviewFixture{failure: &harness.SkillValidationFailure{Code: code, Stage: "quality", Retryable: transient}}
				f := newFixture(t, v)
				g := &fixtureGenerator{id: "feedback-generator"}
				configureGeneration(t, f.service, g)
				jobID, proposalID := "", ""
				if manual {
					configureBehavior(t, f.service, &fixtureBehaviorEvaluator{id: "feedback-behavior"}, behaviorSuite())
					p := queueManualBehavior(t, f, "classified-review")
					jobID, proposalID = p.BehaviorJobID, p.ID
				} else {
					jobID = queueGeneration(t, f, "classified-review").ID
				}
				for attempt := 1; attempt <= attempts; attempt++ {
					generationRun(t, f.service)
					var state string
					var savedAttempts int
					if err := f.db.DB().QueryRow(`SELECT state,attempts FROM df_skill_maintenance_jobs WHERE id=?`, jobID).Scan(&state, &savedAttempts); err != nil {
						t.Fatal(err)
					}
					wantState := "retry"
					if attempt == attempts {
						wantState = "failed"
					}
					if state != wantState || savedAttempts != attempt {
						t.Fatalf("unbounded/invalid review retry: state=%s attempts=%d", state, savedAttempts)
					}
					if _, err := f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, jobID); err != nil {
						t.Fatal(err)
					}
				}
				if !manual {
					job, err := f.service.GetGeneration(context.Background(), jobID)
					if err != nil {
						t.Fatal(err)
					}
					proposalID = job.ProposalID
				}
				p, err := f.service.Get(context.Background(), proposalID)
				if err != nil || p.Status != "failed" || p.ValidationFailure == nil || p.ValidationFailure.Code != code || p.ValidationFailure.Retryable || p.Evaluation.Security != "passed" || len(p.Evaluation.ModelChecks) != 1 {
					t.Fatalf("terminal feedback lost partial evidence/cause or invites more retries: %+v %v", p, err)
				}
				feedback := ProposalFeedback(p)
				if feedback.Quality != "unavailable" || feedback.Failure.Code != code {
					t.Fatalf("terminal tool feedback: %+v", feedback)
				}
				if v.calls != attempts || g.calls.Load() > 1 || f.head(t) != p.Base {
					t.Fatal("review retries regenerated or published the candidate")
				}
			})
		}
	}
}
