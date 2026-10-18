package evolution

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type reviewRetryKey struct{}

func CanRetryReview(p Proposal) bool {
	if p.Status != "failed" || p.Approval != nil || p.ReviewRetryOf != "" || p.Origin != "manual_agent" || p.ValidationFailure == nil {
		return false
	}
	switch p.ValidationFailure.Code {
	case "review_response_invalid":
		return p.ValidationFailure.DetailCode != "version_mismatch" && p.ValidationFailure.DetailCode != "duplicate_field" && p.ValidationFailure.DetailCode != "unexpected_tool_calls" && p.ValidationFailure.DetailCode != "response_too_large"
	case "review_response_incomplete":
		// Legacy incomplete responses did not distinguish refusal from truncation.
		return p.ValidationFailure.DetailCode == "output_truncated"
	case "review_timeout", "review_temporarily_unavailable":
		return true
	}
	return false
}

func (s *Service) ReviewRetryAvailable(p Proposal) bool {
	return s.generation != nil && s.behavior != nil && CanRetryReview(p)
}

// A single deterministic successor preserves the failed record and survives
// duplicate clicks/restarts. Only native budgeted maintenance may retry reviews.
func (s *Service) RetryReview(ctx context.Context, id string, expected int64) (Proposal, error) {
	if s.generation == nil || s.behavior == nil {
		return Proposal{}, invalid("review retry requires native budgeted maintenance")
	}
	var request Request
	var existing Proposal
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.StateVersion != expected || !CanRetryReview(*p) {
			return harness.ErrSkillConflict
		}
		var priorID string
		err := tx.SQL().QueryRowContext(ctx, `SELECT id FROM df_skill_proposals WHERE source_key=? AND name=? AND request_key=?`, p.Target.SourceIdentity, p.SkillName, "review-retry/"+p.ID).Scan(&priorID)
		if err == nil {
			existing, err = readProposal(ctx, tx.SQL(), priorID)
			if err == nil && (existing.ReviewRetryOf != p.ID || existing.CandidateVersion != p.CandidateVersion || existing.Base != p.Base) {
				return harness.ErrSkillConflict
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != p.Base {
			return harness.ErrSkillConflict
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if !skills.SameOverlayBase(overlay, p.OverlayBase) {
			return harness.ErrSkillConflict
		}
		request = Request{SourceID: p.Target.SourceID, Name: p.SkillName, Action: p.Action, Base: p.Base, Reason: p.Reason, RequestKey: "review-retry/" + p.ID, Author: "desktop", Origin: p.Origin, Trigger: p.Trigger}
		if p.Action != "delete" {
			candidate, err := tx.ReadVersion(p.CandidateVersion)
			if err != nil {
				return err
			}
			request.Files = candidate.Files()
			if p.Action != "create" {
				request.Action = "edit"
			}
		}
		return nil
	})
	if err != nil {
		return Proposal{}, err
	}
	if existing.ID != "" {
		return existing, nil
	}
	return s.Submit(context.WithValue(ctx, reviewRetryKey{}, id), request)
}
