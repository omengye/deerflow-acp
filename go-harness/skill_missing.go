package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SubmitMissingSkillReport records an operator's missing-capability judgment
// for an existing root task or descendant. It does not require a skill read,
// establish causality, generate a candidate or change the source run's state.
func (c *Client) SubmitMissingSkillReport(ctx context.Context, request harness.SkillMissingReportRequest) (harness.SkillMissingReportResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillMissingReportResult{}, err
	}
	defer done()
	return c.evolution.SubmitMissingReport(ctx, request)
}

// GetMissingSkillReport resolves the report shared by a real task's root.
// sessionID asserts its foreground root session, including detached sessions.
func (c *Client) GetMissingSkillReport(ctx context.Context, sessionID, runID string) (harness.SkillMissingReportSnapshot, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillMissingReportSnapshot{}, err
	}
	defer done()
	return c.evolution.MissingReport(ctx, sessionID, runID)
}

func (c *Client) ListMissingSkillReports(ctx context.Context, sessionID, cursor string, limit int) (harness.SkillMissingReportPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillMissingReportPage{}, err
	}
	defer done()
	return c.evolution.QueryMissingReports(ctx, sessionID, cursor, limit)
}

func (c *Client) ListMissingSkillReportHistory(ctx context.Context, sessionID, runID, cursor string, limit int) (harness.SkillMissingReportHistoryPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillMissingReportHistoryPage{}, err
	}
	defer done()
	return c.evolution.QueryMissingReportHistory(ctx, sessionID, runID, cursor, limit)
}

// ListMissingSkillReportTargets discovers real root tasks for a foreground
// session, including tasks that have no skill reads or report yet. Results are
// a live, creation-ordered list; they do not identify tasks by last activity.
func (c *Client) ListMissingSkillReportTargets(ctx context.Context, sessionID, cursor string, limit int) (harness.SkillMissingReportTargetPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillMissingReportTargetPage{}, err
	}
	defer done()
	return c.evolution.QueryMissingReportTargets(ctx, sessionID, cursor, limit)
}
