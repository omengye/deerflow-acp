package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// QuerySkillCatalog returns bounded operator metadata, including tombstones.
// A stale publication cursor conflicts; restart pagination from the first page.
func (c *Client) QuerySkillCatalog(ctx context.Context, query harness.SkillQuery) (harness.SkillCatalogPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillCatalogPage{}, err
	}
	defer done()
	return c.skills.QueryCatalog(ctx, query)
}

func (c *Client) GetCatalogSkill(ctx context.Context, target harness.SkillTarget) (harness.SkillCatalogDetail, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillCatalogDetail{}, err
	}
	defer done()
	return c.skills.GetCatalogSkill(ctx, target)
}

// SetSkillEnabledChecked persists operator enablement with a complete expected
// head and request key. Imported content becomes managed against startup resets.
func (c *Client) SetSkillEnabledChecked(ctx context.Context, request harness.SkillEnablement) (harness.SkillManagementResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillManagementResult{}, err
	}
	defer done()
	return c.skills.SetEnabledChecked(ctx, request)
}

func (c *Client) RefreshSkillImport(ctx context.Context, target harness.SkillTarget, expected harness.SkillHead) (harness.SkillCatalogDetail, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillCatalogDetail{}, err
	}
	defer done()
	return c.skills.RefreshCatalogImport(ctx, target, expected)
}

func (c *Client) RetrySkillExport(ctx context.Context, request harness.SkillExportRetry) (harness.SkillManagementResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillManagementResult{}, err
	}
	defer done()
	return c.skills.RetryExportChecked(ctx, request)
}
