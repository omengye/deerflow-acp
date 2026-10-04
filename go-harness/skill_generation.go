package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (c *Client) SkillGenerationStatus(ctx context.Context) (harness.SkillGenerationStatus, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillGenerationStatus{}, err
	}
	defer done()
	if err := ctx.Err(); err != nil {
		return harness.SkillGenerationStatus{}, err
	}
	return c.evolution.GenerationStatus(), nil
}

func (c *Client) PrepareSkillGeneration(ctx context.Context, target harness.SkillTarget) (harness.SkillGenerationPreparation, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillGenerationPreparation{}, err
	}
	defer done()
	return c.evolution.PrepareGeneration(ctx, target)
}

func (c *Client) QueueSkillGeneration(ctx context.Context, r harness.SkillGenerationRequest) (harness.SkillGenerationJob, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillGenerationJob{}, err
	}
	defer done()
	return c.evolution.EnqueueGeneration(ctx, r)
}

func (c *Client) GetSkillGeneration(ctx context.Context, id string) (harness.SkillGenerationJob, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillGenerationJob{}, err
	}
	defer done()
	return c.evolution.GetGeneration(ctx, id)
}

func (c *Client) ListSkillGenerations(ctx context.Context, q harness.SkillQuery) (harness.SkillGenerationPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillGenerationPage{}, err
	}
	defer done()
	return c.evolution.QueryGeneration(ctx, q)
}

func (c *Client) SkillGenerationReceipts(ctx context.Context, id string, after int64, limit int) ([]harness.SkillMaintenanceReceipt, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.evolution.GenerationReceipts(ctx, id, after, limit)
}

func (c *Client) CancelSkillGeneration(ctx context.Context, id string, version int64) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.evolution.CancelGeneration(ctx, id, version)
}
