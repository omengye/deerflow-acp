package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// Only the leased job for this candidate can admit a new installation.
// The safety report is already durable, so a retry never repeats a successful review.
func (s *Service) publishSecurityReviewed(ctx context.Context, id string, expected int64) (Proposal, error) {
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.PublicationPolicy != harness.SkillPublicationSecurityOnly || p.StateVersion != expected {
			return ErrConflict
		}
		if p.Approval != nil {
			return resumeFileDecisionTx(ctx, tx, *p)
		}
		if p.Status != "pending_review" || s.generation == nil {
			return ErrConflict
		}
		if err := s.guardGenerationValidation(ctx, tx, *p); err != nil {
			return err
		}
		if p.Evaluation.ValidatorVersion != s.generation.reviewIdentity || !passedSecurityReview(p.Evaluation, ValidationInput{Proposal: *p}) {
			return ErrValidation
		}
		var retired bool
		if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_evolution_retired_proposals WHERE proposal_id=?)`, id).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return harness.ErrPermissionDenied
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if head != p.Base || !skills.SameOverlayBase(overlay, p.OverlayBase) {
			return skills.ErrPublicationConflict
		}
		candidate, err := tx.ReadVersion(p.CandidateVersion)
		if err != nil {
			return err
		}
		if err = validateCandidateCredentials(candidate, p.Trigger.Type == "experience_pattern"); err != nil {
			return err
		}
		var base map[string][]byte
		if head.Exists {
			previous, err := tx.ReadVersion(head.Version)
			if err != nil {
				return err
			}
			base = previous.Files()
		}
		changed, diff, err := packageDiff(base, candidate.Files())
		if err != nil {
			return err
		}
		if diff != p.Diff || !slices.Equal(changed, p.ChangedFiles) {
			return errors.New("skill publication diff is inconsistent with candidate")
		}
		workspace, err := s.automaticWorkspace(ctx, tx.SQL(), *p)
		if err != nil {
			return err
		}
		workspaceOnly := isDiscoveryTrigger(p.Trigger.Type)
		if !workspaceOnly {
			// Editing a previously scoped version must preserve its audience.
			if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_scoped_versions WHERE source_key=? AND name=? AND version IN (?,?))`, p.Target.SourceIdentity, p.SkillName, head.Version, p.CandidateVersion).Scan(&workspaceOnly); err != nil {
				return err
			}
		}
		if err = ensureBaselinePublication(ctx, tx, head); err != nil {
			return err
		}
		now := time.Now().UTC()
		review := Review{ExpectedStateVersion: expected, Actor: "security_review", Note: "The host safety review passed for this skill package."}
		approval := harness.SkillApproval{SecurityReview: &harness.SkillPublicationSecurityReview{Workspace: workspace, WorkspaceOnly: workspaceOnly, ValidatorVersion: p.Evaluation.ValidatorVersion},
			ProposalID: id, Target: p.Target, Base: p.Base, CandidateVersion: p.CandidateVersion, EvaluationDigest: p.EvaluationDigest, Review: review, CreatedAt: now}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_approvals VALUES(?,?,?)`, id, data, hash(data)); err != nil {
			return err
		}
		enabled := p.Base.Enabled || p.Action == "create"
		next, err := tx.PublishReviewed(approval, enabled)
		if err != nil {
			return err
		}
		publication := Publication{Action: p.Action, Target: p.Target, Name: p.SkillName, ContentVersion: p.CandidateVersion, Enabled: enabled, Previous: head, Head: next, OverlayBase: p.OverlayBase, ProposalID: id, Actor: review.Actor, Note: review.Note, CreatedAt: now}
		publication, err = insertPublication(ctx, tx, publication, approval.FilePublicationKey(), encodedHash(approval))
		if err != nil {
			return err
		}
		p.Status = "published"
		if !workspaceOnly {
			if err = tx.BindPublicationProposal(id); err != nil {
				return err
			}
			p.Status, p.FilePublicationPending = "publishing", true
		}
		p.StateVersion++
		p.PublishedRevision, p.UpdatedAt, p.ReviewedAt = publication.Revision, now, &now
		p.ReviewedBy, p.ReviewNote, p.Approval = review.Actor, review.Note, &approval
		return updateProposal(ctx, tx.SQL(), *p, expected)
	})
	if errors.Is(err, skills.ErrPublicationConflict) {
		err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
			if p.StateVersion != expected || p.Status != "pending_review" || p.Approval != nil {
				return ErrConflict
			}
			p.StateVersion++
			p.Status, p.PublicationError = "stale", "publication_conflict"
			p.Error, p.UpdatedAt = "The reviewed baseline or destination changed before publication.", time.Now().UTC()
			return updateProposal(ctx, tx.SQL(), *p, expected)
		})
	}
	fresh, getErr := s.Get(ctx, id)
	return fresh, errors.Join(err, getErr)
}

func resumeFileDecisionTx(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	if p.Approval == nil || p.Approval.FilePublicationKey() == "" || (p.Status != "published" && p.Status != "publishing") {
		return ErrConflict
	}
	if _, err := publicationByKey(ctx, tx, p.Approval.FilePublicationKey(), encodedHash(*p.Approval)); err != nil {
		return err
	}
	if p.Status == "publishing" {
		return tx.ResumeFilePublication()
	}
	return nil
}

func (s *Service) retryFilePublication(ctx context.Context, id string, version int64) (Proposal, error) {
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Approval == nil || p.Approval.Review.ExpectedStateVersion != version {
			return ErrConflict
		}
		return resumeFileDecisionTx(ctx, tx, *p)
	})
	p, getErr := s.Get(ctx, id)
	return p, errors.Join(err, getErr)
}
