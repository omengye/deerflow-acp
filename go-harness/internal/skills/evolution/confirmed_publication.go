package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// PublishConfirmed bypasses automatic validation/adoption evidence only after
// verifying a native user directive. Decision, cancelled jobs, publication
// history and file journal commit together. No passed checks are manufactured.
func (s *Service) PublishConfirmed(ctx context.Context, id string, req ConfirmedPublicationRequest) (Proposal, error) {
	var result Proposal
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if req.CandidateVersion != p.CandidateVersion || req.ExpectedStateVersion < 1 {
			return ErrConflict
		}
		if p.Author != req.Author || p.Trigger.ThreadID != req.Run.Session.ID {
			return harness.ErrPermissionDenied
		}
		if p.Approval != nil {
			if p.Approval.UserConfirmation == nil {
				return ErrConflict
			}
			// This only resumes an already committed decision for identical
			// bytes; a retry cannot create another revision or new authority.
			result = *p
			return resumeConfirmedTx(ctx, tx, *p)
		}
		if req.ExpectedStateVersion != p.StateVersion || p.Origin != "automatic" || p.CandidateVersion == "" || p.Action == "delete" ||
			(p.Status != "validating" && p.Status != "pending_review" && p.Status != "failed") {
			return ErrConflict
		}
		var retired bool
		if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_evolution_retired_proposals WHERE proposal_id=?)`, id).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return harness.ErrPermissionDenied
		}
		confirmation, err := s.userConfirmationTx(ctx, tx.SQL(), *p, req)
		if err != nil {
			return err
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
			return ErrConflict
		}
		candidate, err := tx.ReadVersion(p.CandidateVersion)
		if err != nil {
			return err
		}
		var base map[string][]byte
		if p.Base.Exists {
			previous, e := tx.ReadVersion(p.Base.Version)
			if e != nil {
				return e
			}
			base = previous.Files()
		}
		changed, diff, err := packageDiff(base, candidate.Files())
		if err != nil {
			return err
		}
		if diff != p.Diff || !slices.Equal(changed, p.ChangedFiles) {
			return errors.New("confirmed skill diff is inconsistent with candidate")
		}
		if err = ensureBaselinePublication(ctx, tx, head); err != nil {
			return err
		}
		// A candidate may still have no model report. Hash its actual empty
		// report for integrity; its checks remain unrun.
		p.EvaluationDigest = encodedHash(p.Evaluation)
		now := time.Now().UTC()
		review := Review{ExpectedStateVersion: p.StateVersion, Actor: "user/" + confirmation.SessionID, Note: "Explicit native user confirmation; automatic evidence gates bypassed. Validation results are unchanged."}
		approval := harness.SkillApproval{UserConfirmation: confirmation, ProposalID: p.ID, Target: p.Target, Base: p.Base, CandidateVersion: p.CandidateVersion, EvaluationDigest: p.EvaluationDigest, Review: review, CreatedAt: now}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_approvals VALUES(?,?,?)`, id, data, hash(data)); err != nil {
			return err
		}
		enabled := p.Base.Enabled || p.Action == "create"
		next, err := tx.PublishConfirmed(approval, enabled)
		if err != nil {
			return err
		}
		publication := Publication{Action: p.Action, Target: p.Target, Name: p.SkillName, ContentVersion: p.CandidateVersion, Enabled: enabled, Previous: head, Head: next, OverlayBase: p.OverlayBase, ProposalID: p.ID, Actor: review.Actor, Note: review.Note, CreatedAt: now}
		publication, err = insertPublication(ctx, tx, publication, "confirm/"+id, encodedHash(approval))
		if err != nil {
			return err
		}
		if err = tx.BindPublicationProposal(id); err != nil {
			return err
		}
		if err = cancelConfirmedJobs(ctx, tx, *p); err != nil {
			return err
		}
		previousState := p.StateVersion
		p.StateVersion++
		p.Status, p.FilePublicationPending = "publishing", true
		p.PublishedRevision, p.UpdatedAt, p.ReviewedAt = publication.Revision, now, &now
		p.ReviewedBy, p.ReviewNote, p.Approval = review.Actor, review.Note, &approval
		// Preserve evaluation and validation failure details as audit evidence.
		p.PublicationError = ""
		if err = updateProposal(ctx, tx.SQL(), *p, previousState); err != nil {
			return err
		}
		result = *p
		return nil
	})
	// File publication finishes after the transaction. Return the durable
	// outcome even when it requires crash recovery or conflict reconciliation.
	if result.ID != "" {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		fresh, e := s.Get(check, id)
		if e == nil {
			result = fresh
		} else {
			result = Proposal{}
			err = errors.Join(err, e)
		}
	}
	return result, err
}

func cancelConfirmedJobs(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	rows, err := tx.SQL().QueryContext(ctx, `SELECT DISTINCT job_id FROM df_skill_generation_inputs WHERE proposal_id=?
 UNION SELECT job_id FROM df_skill_behavior_evaluations WHERE proposal_id=?`, p.ID, p.ID)
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
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		job, err := skillmaintenance.ReadJob(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		if job.State == "queued" || job.State == "running" || job.State == "retry" || job.State == "waiting" {
			if err = skillmaintenance.CancelTx(ctx, tx.SQL(), job.ID, job.StateVersion); err != nil {
				return err
			}
		}
	}
	return nil
}

func resumeConfirmedTx(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	if p.Approval == nil || p.Approval.UserConfirmation == nil || (p.Status != "published" && p.Status != "publishing") {
		return ErrConflict
	}
	if _, err := publicationByKey(ctx, tx, "confirm/"+p.ID, encodedHash(*p.Approval)); err != nil {
		return err
	}
	if p.Status == "publishing" {
		return tx.ResumeFilePublication()
	}
	return nil
}

func (s *Service) retryConfirmedPublication(ctx context.Context, id string, version int64) (Proposal, error) {
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Approval == nil || p.Approval.Review.ExpectedStateVersion != version {
			return ErrConflict
		}
		return resumeConfirmedTx(ctx, tx, *p)
	})
	p, getErr := s.Get(ctx, id)
	return p, errors.Join(err, getErr)
}
