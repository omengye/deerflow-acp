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

func openSkillManageQueue(t *testing.T, ctx context.Context, cfg Config) *Client {
	t.Helper()
	cfg.SkillGeneration = &harness.SkillGenerationOptions{
		Policy:                     harness.SkillMaintenancePolicy{MaxJobSeconds: 60, Workers: 1},
		Sources:                    []string{"project"},
		AllowBuiltinOverlays:       true,
		PublishAfterSecurityReview: true,
	}
	c, err := open(ctx, cfg, skillToolMaintenance{t})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The other SDK fixtures use this validator too. Stable identity allows the
// automatic queue to pin it without starting a maintenance worker in a chat.
func (f skillValidatorFunc) Identity() string { return "fixture" }
