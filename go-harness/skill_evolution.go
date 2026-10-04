package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
)

// NewSkillModelValidator constructs the explicit content-review dependency
// for Config.SkillValidator. It performs two isolated, bounded calls per
// evaluation, with no tools or conversation history. It does not run behavioral
// tests, authorize scripts or enable discovery/automatic publication.
func NewSkillModelValidator(ctx context.Context, config harness.SkillModelReviewConfig) (harness.SkillValidator, error) {
	return einoengine.NewSkillModelValidator(ctx, config)
}

// NewSkillBehaviorEvaluator constructs the bounded Eino fixture runner. It
// exposes only local text files and host-defined simulated tools; every model
// call requires the maintenance budget supplied by the evolution worker.
func NewSkillBehaviorEvaluator(ctx context.Context, config harness.SkillBehaviorModelConfig) (harness.SkillBehaviorEvaluator, error) {
	return einoengine.NewSkillBehaviorEvaluator(ctx, config)
}

// These are operator APIs over explicitly configured sources. They are not
// model tools and never infer publication authority from a model-supplied role.
func (c *Client) GetSkillHead(ctx context.Context, source, name string) (harness.SkillHead, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillHead{}, err
	}
	defer done()
	_, head, _, err := c.evolution.Head(ctx, source, name)
	return head, err
}

func (c *Client) SubmitSkillProposal(ctx context.Context, request harness.SkillProposalRequest) (harness.SkillProposal, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillProposal{}, err
	}
	defer done()
	return c.evolution.Submit(ctx, request)
}

func (c *Client) GetSkillProposal(ctx context.Context, id string) (harness.SkillProposal, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillProposal{}, err
	}
	defer done()
	return c.evolution.Get(ctx, id)
}

func (c *Client) ListSkillProposals(ctx context.Context, query harness.SkillQuery) (harness.SkillProposalPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillProposalPage{}, err
	}
	defer done()
	return c.evolution.QueryProposals(ctx, query)
}

func (c *Client) ApproveSkillProposal(ctx context.Context, id string, review harness.SkillReview) (harness.SkillProposal, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillProposal{}, err
	}
	defer done()
	return c.evolution.Approve(ctx, id, review)
}

func (c *Client) RejectSkillProposal(ctx context.Context, id string, review harness.SkillReview) (harness.SkillProposal, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillProposal{}, err
	}
	defer done()
	return c.evolution.Reject(ctx, id, review)
}

func (c *Client) RollbackSkill(ctx context.Context, request harness.SkillRollback) (harness.SkillPublication, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillPublication{}, err
	}
	defer done()
	return c.evolution.Rollback(ctx, request)
}

func (c *Client) ListSkillHistory(ctx context.Context, query harness.SkillQuery) (harness.SkillHistoryPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.SkillHistoryPage{}, err
	}
	defer done()
	return c.evolution.QueryHistory(ctx, query)
}
