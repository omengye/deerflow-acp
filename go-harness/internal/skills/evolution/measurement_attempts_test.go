package evolution

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type unavailableMeasurement struct{ fixtureBehaviorEvaluator }

func (e *unavailableMeasurement) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	r, err := e.fixtureBehaviorEvaluator.Evaluate(ctx, in)
	if err == nil && in.Variant == "candidate" {
		r.Passed, r.StopReason, r.ErrorCode = false, "failed", "timeout"
		for i := range r.Checks {
			r.Checks[i].Passed = false
		}
	}
	return r, err
}

func TestMeasurementRetriesAreFiniteAndReuseCompletedBaselineAfterRestart(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	e := &unavailableMeasurement{fixtureBehaviorEvaluator: fixtureBehaviorEvaluator{id: "unavailable-fixture"}}
	if err := f.service.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{behaviorSuite()}}); err != nil {
		t.Fatal(err)
	}
	p := queueManualBehavior(t, f, "finite-measurement")
	for attempt := 1; attempt <= 3; attempt++ {
		generationRun(t, f.service)
		got, err := f.service.Get(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			if got.Status != "validating" {
				t.Fatal("transient measurement became a quality decision", got.Status)
			}
			next, err := New(ctx, f.registry, v)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			configureGeneration(t, next, g)
			if err = next.ConfigureBehavior(e, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{behaviorSuite()}}); err != nil {
				t.Fatal(err)
			}
			f.service = next
			if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, p.BehaviorJobID); err != nil {
				t.Fatal(err)
			}
		} else if got.Status != "failed" || got.Evaluation.BehaviorReport == nil || got.Evaluation.BehaviorReport.MeasurementErrors != 1 {
			t.Fatal("exhausted measurement was adopted or lost", got.Status, got.Evaluation.Behavior)
		}
	}
	var attempts int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_measurement_attempts`).Scan(&attempts); err != nil || attempts != 3 || e.calls.Load() != 4 || v.calls.Load() != 1 {
		t.Fatal("retry counter or cached baseline lost", attempts, e.calls.Load(), v.calls.Load(), err)
	}
	if worked, err := f.service.RunGeneration(ctx); err != nil || worked {
		t.Fatal("exhausted measurement retried", worked, err)
	}
}
