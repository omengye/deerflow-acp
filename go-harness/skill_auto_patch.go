package deerflow

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (c *Client) SkillAutoPatchStatus(ctx context.Context) (harness.SkillAutoPatchStatus, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillAutoPatchStatus{}, err
	}
	defer done()
	return c.evolution.AutoPatchStatus(ctx)
}

func (c *Client) ListSkillAutoPatchObservations(ctx context.Context, q harness.SkillQuery) (harness.SkillAutoPatchPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillAutoPatchPage{}, err
	}
	defer done()
	return c.evolution.QueryAutoPatch(ctx, q)
}
