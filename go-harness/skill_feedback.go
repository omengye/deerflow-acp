package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SubmitSkillFeedback records an operator judgment about observed skill reads.
// It does not run a model, publish a skill or replay the source conversation.
func (c *Client) SubmitSkillFeedback(ctx context.Context, request harness.SkillFeedbackRequest) (harness.SkillFeedbackResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillFeedbackResult{}, err
	}
	defer done()
	return c.evolution.SubmitFeedback(ctx, request)
}

func (c *Client) GetSkillFeedback(ctx context.Context, signalID string) (harness.SkillFeedbackSnapshot, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillFeedbackSnapshot{}, err
	}
	defer done()
	return c.evolution.Feedback(ctx, signalID)
}

func (c *Client) ListSkillFeedback(ctx context.Context, signalID, cursor string, limit int) (harness.SkillFeedbackPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillFeedbackPage{}, err
	}
	defer done()
	return c.evolution.QueryFeedback(ctx, signalID, cursor, limit)
}
