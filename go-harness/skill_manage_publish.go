package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func (t *skillManageTool) publishConfirmed(ctx context.Context, input skillManageInput, author string, role *agents.Invocation) (string, error) {
	if role != nil {
		return skillManageFailure("permission_denied", "Only the lead agent can publish using the native user's confirmation.")
	}
	if input.ProposalID == "" || input.CandidateSHA == "" || input.StateVersion < 1 || input.Name != "" || input.Source != "" || input.Reason != "" || input.Content != nil || input.Files != nil || input.Path != "" || input.Find != nil || input.Replace != nil || input.ExpectedCount != nil {
		return skillManageFailure("invalid_request", "publish accepts only proposal_id, candidate_sha256 and state_version from get. It cannot change candidate content or accept an agent-authored confirmation.")
	}
	p, err := t.service.Get(ctx, input.ProposalID)
	if err != nil || !t.owns(p, author, nil) || slices.Contains(t.builtin, p.Target.SourceID) {
		if err != nil && !errors.Is(err, harness.ErrNotFound) && !errors.Is(err, harness.ErrInvalidInput) && !errors.Is(err, harness.ErrPermissionDenied) {
			return "", harness.MarkToolNoEffect(err)
		}
		return skillManageFailure("not_found", "Proposal is not available for publication by this session.")
	}
	wasPublished := p.Status == "published"
	p, err = t.service.PublishConfirmed(ctx, p.ID, evolution.ConfirmedPublicationRequest{Run: t.run, Author: author, CandidateVersion: input.CandidateSHA, ExpectedStateVersion: input.StateVersion})
	if err == nil || p.Approval != nil && p.Approval.UserConfirmation != nil {
		var result map[string]any
		_ = json.Unmarshal([]byte(t.result(ctx, p, nil)), &result)
		result["live_skill_changed"] = !wasPublished && p.Status == "published"
		if err != nil {
			// A committed file journal may need recovery. Never report a
			// no-effect rejection after persisting the user's decision.
			result["publication_recovery_required"] = true
			result["next_action"] = "The exact publication decision is recorded. Inspect get and retry publish with its binding to resume file publication; do not create another candidate."
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	}
	if errors.Is(err, evolution.ErrUserConfirmation) {
		return skillManageFailure("user_confirmation_required", "No unambiguous publication confirmation for this candidate was found in the current native user message. A message such as 确认发布这个技能，跳过证据门槛 authorizes the latest candidate for one skill; name the skill or proposal ID when several exist. Editing requests and quoted text do not authorize publication.")
	}
	if errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, skills.ErrPublicationConflict) {
		return skillManageFailure("conflict", "The candidate or installed base changed. Refresh get and inspect the current head; this confirmation cannot overwrite a newer version.")
	}
	if errors.Is(err, harness.ErrPermissionDenied) {
		return skillManageFailure("permission_denied", "The native user input, candidate ownership, workspace or source does not authorize this publication.")
	}
	return "", err
}
