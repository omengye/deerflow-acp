package evolution

import (
	"context"
	"fmt"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBehaviorStreams33SuitesAndCompletesAll35Cases(t *testing.T) {
	f, _, _ := generationFixture(t)
	e := &fixtureBehaviorEvaluator{id: "capacity-evaluator"}
	err := f.service.ConfigureBehavior(e, harness.SkillBehaviorOptions{Load: func(visit func(harness.SkillBehaviorSuite) error) error {
		for i := 0; i < 33; i++ {
			suite := behaviorSuite()
			suite.ID = fmt.Sprintf("suite-%d", i)
			if i != 32 {
				suite.Name = fmt.Sprintf("skill-%d", i)
			}
			if i == 32 {
				template := suite.Cases[0]
				suite.Cases = nil
				for j := 0; j < 35; j++ {
					c := template
					c.ID = fmt.Sprintf("case-%02d", j)
					suite.Cases = append(suite.Cases, c)
				}
			}
			if err := visit(suite); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal("suite directory capacity blocked", err)
	}
	for _, binding := range f.service.behavior.suites {
		if len(binding.suite.Cases) != 0 {
			t.Fatal("suite payload retained outside durable manifest")
		}
	}
	p := queueManualBehavior(t, f, "all-cases")
	for slice := 0; slice < 10; slice++ {
		generationRun(t, f.service)
		got, err := f.service.Get(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "validating" {
			if got.Evaluation.BehaviorReport == nil || got.Evaluation.BehaviorReport.TotalComparisons != 35 || got.Evaluation.Behavior != "passed" || e.calls.Load() != 70 {
				t.Fatal("truncated suite was treated as complete", got.Evaluation.Behavior, e.calls.Load())
			}
			return
		}
	}
	t.Fatal("case pagination did not finish")
}
