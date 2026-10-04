package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (c *Client) ListSkillSignals(ctx context.Context, query harness.SkillQuery) (harness.SkillSignalPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillSignalPage{}, err
	}
	defer done()
	return c.evolution.QuerySignals(ctx, query)
}

func (c *Client) GetSkillSignalEvidence(ctx context.Context, id, cursor string, limit int) (harness.SkillSignalEvidence, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillSignalEvidence{}, err
	}
	defer done()
	return c.evolution.SignalEvidence(ctx, id, cursor, limit)
}

func (c *Client) SkillCollectorStatus(ctx context.Context) (harness.SkillCollectorStatus, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillCollectorStatus{}, err
	}
	defer done()
	return c.evolution.CollectorStatus(ctx)
}

func (c *Client) ListSkillCollectionJobs(ctx context.Context, after int64, limit int) (harness.SkillCollectionJobPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillCollectionJobPage{}, err
	}
	defer done()
	return c.evolution.QueryCollectionJobs(ctx, after, limit)
}

// RetrySkillCollection restarts only a failed local evidence page. It does not
// rerun any user tool, model, candidate evaluation or publication.
func (c *Client) RetrySkillCollection(ctx context.Context, outcomeID, expectedVersion int64) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.evolution.RetryCollection(ctx, outcomeID, expectedVersion)
}
