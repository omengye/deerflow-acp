package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Source inspection is read-only host authority, not a conversation/model tool.
func (c *Client) SkillLearningSource(ctx context.Context, sessionID string, request harness.SkillLearningSourceRequest) (harness.SkillLearningSource, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningSource{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillLearningSource{}, err
	}
	return c.learning.Source(ctx, sessionID, request)
}

// Workspace learning queries require trusted host authority. They deliberately
// bypass no ownership boundary through a single-session ACP extension.
func (c *Client) WorkspaceSkillExperiencePatterns(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePatternPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillExperiencePatternPage{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillExperiencePatternPage{}, err
	}
	return c.learning.WorkspacePatterns(ctx, sessionID, after, limit)
}

func (c *Client) WorkspaceSkillExperienceMatches(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperienceMatchPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillExperienceMatchPage{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillExperienceMatchPage{}, err
	}
	return c.learning.WorkspaceMatches(ctx, sessionID, after, limit)
}

func (c *Client) LinkSkillLearningEpisode(ctx context.Context, sessionID string, req harness.SkillLearningLineageRequest) (string, error) {
	done, err := c.operation()
	if err != nil {
		return "", err
	}
	defer done()
	for _, id := range []string{sessionID, req.SourceSessionID} {
		if _, err = c.service.Store.ManagementSession(ctx, id); err != nil {
			return "", err
		}
	}
	return c.learning.LinkEpisodeLineage(ctx, sessionID, req)
}

// Suggestion review is trusted management authority, not an ACP model tool.
func (c *Client) SetSkillLearningPatternIgnored(ctx context.Context, sessionID string, request harness.SkillLearningPatternIgnoreRequest) (harness.SkillLearningPatternReviewResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningPatternReviewResult{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillLearningPatternReviewResult{}, err
	}
	return c.learning.SetPatternIgnored(ctx, sessionID, request)
}

func (c *Client) AdjudicateSkillLearningPattern(ctx context.Context, sessionID string, request harness.SkillLearningPatternAdjudicationRequest) (harness.SkillLearningPatternReviewResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningPatternReviewResult{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillLearningPatternReviewResult{}, err
	}
	return c.learning.AdjudicatePattern(ctx, sessionID, request)
}

func (c *Client) PrepareSkillLearningNewSkillRetry(ctx context.Context, sessionID string, request harness.SkillLearningNewSkillRetryPreparationRequest) (harness.SkillLearningNewSkillRetryPreparation, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningNewSkillRetryPreparation{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillLearningNewSkillRetryPreparation{}, err
	}
	if c.evolution == nil {
		return harness.SkillLearningNewSkillRetryPreparation{}, harness.ErrPermissionDenied
	}
	return c.evolution.PrepareNewSkillRetry(ctx, sessionID, request)
}

func (c *Client) RetrySkillLearningNewSkill(ctx context.Context, sessionID string, request harness.SkillLearningNewSkillRetryRequest) (harness.SkillLearningNewSkillRetryResult, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningNewSkillRetryResult{}, err
	}
	defer done()
	if _, err = c.service.Store.ManagementSession(ctx, sessionID); err != nil {
		return harness.SkillLearningNewSkillRetryResult{}, err
	}
	if c.evolution == nil {
		return harness.SkillLearningNewSkillRetryResult{}, harness.ErrPermissionDenied
	}
	return c.retrySkillLearningNewSkill(ctx, sessionID, request)
}

func (c *Client) retrySkillLearningNewSkill(ctx context.Context, sessionID string, request harness.SkillLearningNewSkillRetryRequest) (harness.SkillLearningNewSkillRetryResult, error) {
	// Historical idempotency does not require generation or model adapters on a
	// disabled host. This read-only store path cannot mint a new authorization.
	saved, err := c.learning.NewSkillRetryReceipt(ctx, sessionID, request)
	if err != nil || saved != nil {
		if saved != nil {
			return *saved, err
		}
		return harness.SkillLearningNewSkillRetryResult{}, err
	}
	return c.evolution.RetryNewSkill(ctx, sessionID, request)
}

func (c *Client) SkillLearningStatus(ctx context.Context) (harness.SkillLearningStatus, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningStatus{}, err
	}
	defer done()
	return c.skillLearningStatus(ctx)
}

func (c *Client) skillLearningStatus(ctx context.Context) (harness.SkillLearningStatus, error) {
	status, err := c.learning.Status(ctx)
	if err != nil || c.evolution == nil {
		return status, err
	}
	discovery, err := c.evolution.DiscoveryStatus(ctx)
	if err != nil {
		return status, err
	}
	status.ProposalDiscoveryEnabled = discovery.Enabled && discovery.Options != nil && discovery.Options.ExperiencePatternsEnabled
	if status.ProposalDiscoveryEnabled {
		status.Mode = "review_candidates"
	}
	patch, err := c.evolution.AutoPatchStatus(ctx)
	status.AutoPatchEnabled = status.ProposalDiscoveryEnabled && patch.Enabled && patch.Options != nil && patch.Options.ExperiencePatternsEnabled
	if status.AutoPatchEnabled {
		status.Mode = "guarded_auto_patch"
	}
	return status, err
}

func (c *Client) SkillLearningControl(ctx context.Context, sessionID string) (harness.SkillLearningControl, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningControl{}, err
	}
	defer done()
	return c.service.SkillLearningControl(ctx, c.owner, sessionID)
}

func (c *Client) SetSkillLearningControl(ctx context.Context, sessionID string, request harness.SkillLearningControlRequest) (harness.SkillLearningControl, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningControl{}, err
	}
	defer done()
	return c.service.SetSkillLearningControl(ctx, c.owner, sessionID, request)
}

func (c *Client) SkillExperiences(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillExperiencePage{}, err
	}
	defer done()
	return c.service.SkillExperiences(ctx, c.owner, sessionID, after, limit)
}

func (c *Client) SkillExperiencePatterns(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePatternPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillExperiencePatternPage{}, err
	}
	defer done()
	return c.service.SkillExperiencePatterns(ctx, c.owner, sessionID, after, limit)
}

func (c *Client) SkillExperienceMatches(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperienceMatchPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillExperienceMatchPage{}, err
	}
	defer done()
	return c.service.SkillExperienceMatches(ctx, c.owner, sessionID, after, limit)
}

func (c *Client) StartSkillLearningEpisode(ctx context.Context, sessionID string) (string, error) {
	done, err := c.operation()
	if err != nil {
		return "", err
	}
	defer done()
	return c.service.StartSkillLearningEpisode(ctx, c.owner, sessionID)
}

func (c *Client) SkillLearningJobs(ctx context.Context, sessionID, after string, limit int) (harness.SkillLearningJobPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillLearningJobPage{}, err
	}
	defer done()
	return c.service.SkillLearningJobs(ctx, c.owner, sessionID, after, limit)
}

func (c *Client) CancelSkillLearningExtraction(ctx context.Context, sessionID, jobID string, version int64) error {
	done, err := c.operation()
	if err != nil {
		return err
	}
	defer done()
	return c.service.CancelSkillLearningExtraction(ctx, c.owner, sessionID, jobID, version)
}
