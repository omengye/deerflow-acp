package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// Persist the operator's exact decision before a separate publication can fail.
// Replays must carry the original review, never today's proposal version.
func (s *Service) recordApproval(ctx context.Context, id string, review Review) error {
	var outcome error
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Approval != nil {
			if p.Approval.Review != review {
				return ErrConflict
			}
			return nil
		}
		// Older successful publications retain their existing idempotency key.
		if p.Status == "published" || p.Status == "publishing" {
			_, err := publicationByKey(ctx, tx, "approve/"+id, encodedHash(review))
			return err
		}
		if p.StateVersion != review.ExpectedStateVersion || p.Status != "pending_review" {
			return ErrConflict
		}
		if err := s.guardDiscoveryApproval(ctx, tx, *p); err != nil {
			if errors.Is(err, errDiscoveryEvidence) || errors.Is(err, errExperienceCredential) {
				p.StateVersion++
				p.Status, p.Error = "stale", "The discovery evidence changed or was withdrawn."
				outcome = ErrConflict
				if errors.Is(err, errExperienceCredential) {
					p.Status, p.Error = "failed", "The generated skill contains a credential-like literal."
					outcome = ErrValidation
				}
				p.UpdatedAt = time.Now().UTC()
				return updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion)
			}
			return err
		}
		input := ValidationInput{Proposal: *p}
		for version, dest := range map[string]*map[string][]byte{p.Base.Version: &input.BaseFiles, p.CandidateVersion: &input.CandidateFiles} {
			if version != "" {
				candidate, err := tx.ReadVersion(version)
				if err != nil {
					return err
				}
				*dest = candidate.Files()
			}
		}
		if !validReportFor(p.Evaluation, input) {
			return ErrValidation
		}
		if err := s.guardAutoPatch(ctx, tx, *p, input); err != nil {
			return err
		}
		if requiresScriptAcknowledgment(p.Evaluation, input) && !review.AcknowledgeUnverifiedScripts {
			return invalid("publication requires explicit acknowledgment that support scripts were not execution-validated")
		}
		if err := s.guardBehaviorReport(ctx, tx.SQL(), *p, p.Evaluation); err != nil {
			return err
		}
		changed, diff, err := packageDiff(input.BaseFiles, input.CandidateFiles)
		if err != nil {
			return err
		}
		if diff != p.Diff || !slices.Equal(changed, p.ChangedFiles) {
			return errors.New("skill approval diff is inconsistent")
		}
		approval := harness.SkillApproval{ProposalID: p.ID, Target: p.Target, Base: p.Base, CandidateVersion: p.CandidateVersion, EvaluationDigest: p.EvaluationDigest, Review: review, CreatedAt: time.Now().UTC()}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_approvals VALUES(?,?,?)`, id, data, hash(data))
		return err
	})
	if err != nil {
		return err
	}
	return outcome
}

// RetryPublication resumes the original decision without changing its actor or
// content. Automatic decisions still pass the current automatic-policy gates.
func (s *Service) RetryPublication(ctx context.Context, id string, approvalVersion int64) (Proposal, error) {
	p, err := s.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	if p.Approval == nil || p.Approval.Review.ExpectedStateVersion != approvalVersion {
		return Proposal{}, ErrConflict
	}
	if p.Approval.Review.Actor == "auto_patch" {
		ctx = context.WithValue(ctx, autoPatchContextKey{}, true)
	}
	return s.Approve(ctx, id, p.Approval.Review)
}

// RecoverApprovals closes the crash window between the durable decision and
// its publication journal. It never calls review models or approves new bytes.
// Failed work remains inspectable; one bad source must not disable the host.
func (s *Service) RecoverApprovals(ctx context.Context) error {
	var cursor string
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT p.id FROM df_skill_proposals p JOIN df_skill_approvals a ON a.proposal_id=p.id WHERE p.status='pending_review' AND p.id>? ORDER BY p.id LIMIT 100`, cursor)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			// Validate stored decisions even if their source is no longer configured.
			p, err := readProposal(ctx, s.db, id)
			if err != nil {
				return err
			}
			if p.Approval != nil {
				if _, err = s.RetryPublication(ctx, id, p.Approval.Review.ExpectedStateVersion); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					slog.Warn("approved skill publication requires attention", "proposal_id", id)
				}
			}
			cursor = id
		}
		if len(ids) < 100 {
			return nil
		}
	}
}
