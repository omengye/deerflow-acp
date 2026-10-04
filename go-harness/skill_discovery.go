package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SkillDiscoveryStatus reports the applied host's opt-in, limits and durable
// reservations. Operator corrections remain unassessed quality evidence.
func (c *Client) SkillDiscoveryStatus(ctx context.Context) (harness.SkillDiscoveryStatus, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillDiscoveryStatus{}, err
	}
	defer done()
	return c.evolution.DiscoveryStatus(ctx)
}

func (c *Client) ListSkillDiscoveries(ctx context.Context, q harness.SkillQuery) (harness.SkillDiscoveryPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillDiscoveryPage{}, err
	}
	defer done()
	return c.evolution.QueryDiscoveries(ctx, q)
}
