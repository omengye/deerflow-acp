package launch

import (
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestLongTaskSafetyReplacesOnlyDisabledCallQuotas(t *testing.T) {
	for _, tc := range []struct {
		name                string
		budget              *harness.BudgetLimits
		wantModel, wantTool int
	}{
		{"nil", nil, harness.LongTaskSafetyModelCalls, harness.LongTaskSafetyToolCalls},
		{"zero", &harness.BudgetLimits{}, harness.LongTaskSafetyModelCalls, harness.LongTaskSafetyToolCalls},
		{"explicit", &harness.BudgetLimits{MaxModelCalls: 50, MaxToolCalls: 90000}, 50, 90000},
	} {
		cfg := deerflow.Config{Budget: tc.budget}
		ApplyLongTaskSafety(&cfg)
		if cfg.Budget.MaxModelCalls != tc.wantModel || cfg.Budget.MaxToolCalls != tc.wantTool {
			t.Fatalf("%s: %+v", tc.name, cfg.Budget)
		}
	}
	cfg := deerflow.Config{Budget: &harness.BudgetLimits{MaxTokens: 7, MaxOutputTokens: 512}}
	ApplyLongTaskSafety(&cfg)
	if cfg.Budget.MaxTokens != 7 || cfg.Budget.MaxOutputTokens != 512 || cfg.Budget.Timeout != 0 {
		t.Fatalf("unrelated limits changed: %+v", cfg.Budget)
	}
}
