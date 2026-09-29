package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// InstallSkill captures one package from an explicitly configured source.
// New installations are disabled until the host reviews and enables them.
func (c *Client) InstallSkill(ctx context.Context, source, relativeDir string) (harness.SkillRecord, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillRecord{}, err
	}
	defer done()
	return c.skills.Install(ctx, source, relativeDir)
}

// SetSkillEnabled affects future runs. Existing runs/checkpoints retain their
// immutable snapshot; remove a source from host configuration to revoke it.
func (c *Client) SetSkillEnabled(ctx context.Context, source, name string, enabled bool) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.skills.SetEnabled(ctx, source, name, enabled)
}

func (c *Client) DeleteSkill(ctx context.Context, source, name string) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.skills.Delete(ctx, source, name)
}

// ListSkills is an operator API over configured sources, including disabled
// installations. Run selection always derives its workspace from the session.
func (c *Client) ListSkills(ctx context.Context, selection harness.SkillSelection) ([]harness.SkillRecord, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	return c.skills.List(ctx, selection)
}
