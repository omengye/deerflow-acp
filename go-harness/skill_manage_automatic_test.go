package deerflow

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type skillToolMaintenance struct{ t *testing.T }

func (f skillToolMaintenance) Identity() string { return "tool-maintenance-fixture-v1" }
func (f skillToolMaintenance) Generate(context.Context, harness.SkillGenerationInput) (harness.SkillGeneratedCandidate, error) {
	f.t.Error("submitted bytes must not be regenerated")
	return harness.SkillGeneratedCandidate{}, harness.ErrPermissionDenied
}
func (f skillToolMaintenance) Evaluate(context.Context, harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	f.t.Error("submission must not run behavioral evaluation synchronously")
	return harness.SkillBehaviorResult{}, harness.ErrPermissionDenied
}

func configureSkillManageQueue(t *testing.T, c *Client) {
	t.Helper()
	fixture := skillToolMaintenance{t}
	if err := c.evolution.ConfigureGeneration(context.Background(), fixture, harness.SkillMaintenancePolicy{MaxJobSeconds: 60, Workers: 1}, []string{"project"}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.evolution.ConfigureBehavior(fixture, harness.SkillBehaviorOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.evolution.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 100, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
		t.Fatal(err)
	}
}

// The other SDK fixtures use this validator too. Stable identity allows the
// automatic queue to pin it without starting a maintenance worker in a chat.
func (f skillValidatorFunc) Identity() string { return "fixture" }
