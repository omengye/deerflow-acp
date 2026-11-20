package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

// Older clients may still send publish. Only a previously admitted file
// journal can be resumed here. A pending or rejected safety review is final
// for this call, regardless of the wording of the session input.
func (t *skillManageTool) publicationStatus(ctx context.Context, input skillManageInput, author string, role *agents.Invocation) (string, error) {
	if role != nil {
		return skillManageFailure("permission_denied", "Only the lead agent can resume a file publication.")
	}
	if input.ProposalID == "" || input.CandidateSHA == "" || input.StateVersion < 1 || input.Name != "" || input.Source != "" || input.Reason != "" || input.Content != nil || input.Files != nil || input.Path != "" || input.Find != nil || input.Replace != nil || input.ExpectedCount != nil {
		return skillManageFailure("invalid_request", "publish accepts only the proposal_id, candidate_sha256 and state_version returned by get.")
	}
	p, err := t.service.Get(ctx, input.ProposalID)
	if err != nil || !t.owns(p, author, nil) || p.Trigger.ThreadID != t.run.Session.ID || slices.Contains(t.builtin, p.Target.SourceID) {
		if err != nil && !errors.Is(err, harness.ErrNotFound) && !errors.Is(err, harness.ErrInvalidInput) && !errors.Is(err, harness.ErrPermissionDenied) {
			return "", harness.MarkToolNoEffect(err)
		}
		return skillManageFailure("not_found", "This session cannot access this proposal.")
	}
	if input.CandidateSHA != p.CandidateVersion || (input.StateVersion != p.StateVersion && (p.Approval == nil || input.StateVersion != p.Approval.Review.ExpectedStateVersion)) {
		return skillManageFailure("conflict", "The candidate or state version changed. Refresh get.")
	}
	if p.Status != "publishing" || p.Approval == nil || p.Approval.FilePublicationKey() == "" {
		return t.result(ctx, p, role), nil
	}
	fresh, err := t.service.RetryPublication(ctx, p.ID, p.Approval.Review.ExpectedStateVersion)
	if fresh.ID != "" {
		p = fresh
	}
	var result map[string]any
	_ = json.Unmarshal([]byte(t.result(ctx, p, role)), &result)
	result["live_skill_changed"] = p.Status == "published"
	if err != nil {
		result["publication_recovery_required"] = true
		result["next_action"] = "The admitted file publication needs recovery. Inspect this proposal and retry its exact publication."
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}
