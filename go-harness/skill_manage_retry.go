package deerflow

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (t *skillManageTool) retryReview(ctx context.Context, original harness.SkillProposal, expected int64, author string, role *agents.Invocation) (string, error) {
	if original.StateVersion != expected {
		return skillManageFailure("conflict", "Proposal state changed. Use get to inspect its current state before requesting a review retry.")
	}
	if !t.service.ReviewRetryAvailable(original) {
		return skillManageFailure("review_retry_unavailable", "This proposal cannot be retried under the host's current review policy and budgeted maintenance configuration. Inspect get for validation_failure and next_action; do not create an unchanged replacement or change source to bypass a failed review.")
	}
	p, retryErr := t.service.RetryReview(ctx, original.ID, expected)
	if retryErr == nil {
		if !t.owns(p, author, role) {
			return skillManageFailure("not_found", "The existing retry belongs to a different actor. Ask the operator to inspect its status; do not create another retry.")
		}
		return t.result(ctx, p, role), nil
	}
	if p.ID != "" {
		// A cancelled response may follow a committed successor. Recover its
		// deterministic request identity without spending another review budget.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saved, err := t.service.LookupRequest(check, original.Target.SourceID, original.SkillName, "review-retry/"+original.ID)
		if err == nil && t.owns(saved, author, role) {
			return t.result(ctx, saved, role), nil
		}
		if err != nil && !errors.Is(err, harness.ErrNotFound) {
			return "", err
		}
	}
	if errors.Is(retryErr, harness.ErrSkillConflict) || errors.Is(retryErr, skills.ErrPublicationConflict) {
		return skillManageFailure("conflict", "The proposal or its source changed. Inspect the current proposal and source; retry cannot adopt different content or an old approval.")
	}
	if errors.Is(retryErr, harness.ErrInvalidInput) || errors.Is(retryErr, harness.ErrPermissionDenied) {
		return skillManageFailure("review_retry_unavailable", "The host cannot retry this proposal under its current review configuration and source permissions. Ask the operator to inspect it; no approval was granted.")
	}
	return "", retryErr
}
