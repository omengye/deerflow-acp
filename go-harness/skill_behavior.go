package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SkillBehaviorEvaluation reads the persisted fixture execution for a proposal.
// The response omits fixture input, expected answers and model transcripts.
func (c *Client) SkillBehaviorEvaluation(ctx context.Context, proposalID string) (harness.SkillBehaviorEvaluation, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillBehaviorEvaluation{}, err
	}
	defer done()
	return c.evolution.BehaviorEvaluation(ctx, proposalID)
}

func (c *Client) SkillBehaviorReceipts(ctx context.Context, proposalID string, after int64, limit int) ([]harness.SkillMaintenanceReceipt, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.evolution.BehaviorReceipts(ctx, proposalID, after, limit)
}
