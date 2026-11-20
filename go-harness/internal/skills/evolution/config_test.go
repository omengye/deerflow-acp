package evolution

import (
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"reflect"
	"testing"
)

func TestAutomaticEvolutionInheritsOnlyPrimaryModel(t *testing.T) {
	for _, model := range []string{"primary", "second"} {
		s, err := DecodeReviewConfig(nil, map[string]bool{"primary": true, "second": true}, model)
		if err != nil || s.PrimaryModel != model {
			t.Fatal(s, err)
		}
		if !s.GenerationOptions(harness.ModelRoute{}, []string{"managed-evolution"}).PublishAfterSecurityReview || s.LearningOptions(harness.ModelRoute{}) == nil || s.DiscoveryOptions() == nil {
			t.Fatal("automatic stage missing")
		}
		if !reflect.DeepEqual(s.Policy, AutomaticSettings(model).Policy) {
			t.Fatal("unstable policy")
		}
	}
	for _, old := range []string{"enabled: false", "mode: review", "budget: {max_daily_model_calls: 1}", "learning: {enabled: false}", "generation_model_name: second"} {
		if _, err := DecodeReviewConfig([]byte(old), map[string]bool{"primary": true}, "primary"); err == nil {
			t.Fatal("old policy accepted", old)
		}
	}
	if _, err := DecodeReviewConfig(nil, map[string]bool{"primary": true}, "missing"); err == nil {
		t.Fatal("missing primary route accepted")
	}
}
