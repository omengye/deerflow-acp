package evolution

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// CandidateProgress translates historical storage states into automatic
// lifecycle states. A pending_review row is never a request for human approval.
type CandidateProgress struct {
	Evidence       *harness.SkillBehaviorEvidence `json:"evidence,omitempty"`
	Status         string                         `json:"status"`
	Reason         string                         `json:"reason,omitempty"`
	JobID          string                         `json:"job_id,omitempty"`
	JobState       string                         `json:"job_state,omitempty"`
	AutomaticRetry bool                           `json:"automatic_retry"`
}

func CandidateStatus(p Proposal) string {
	if p.Approval != nil && p.Approval.UserConfirmation != nil {
		return p.Status
	}
	if p.Origin != "automatic" {
		return "historical"
	}
	switch p.Status {
	case "validating", "generating":
		return "evaluating"
	case "pending_review":
		return "awaiting_adoption"
	case "published":
		return "adopted"
	case "failed", "rejected":
		return "rejected"
	case "stale":
		return "superseded"
	default:
		return p.Status
	}
}

func (s *Service) CandidateProgress(ctx context.Context, id string) (out CandidateProgress, err error) {
	err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		out.Status = CandidateStatus(*p)
		if p.Approval != nil && p.Approval.UserConfirmation != nil {
			out.Reason = "user_confirmed"
			return nil
		}
		var retired bool
		if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_evolution_retired_proposals WHERE proposal_id=?)`, id).Scan(&retired); err != nil {
			return err
		}
		if retired || p.Origin != "automatic" {
			out.Status, out.Reason = "historical", "retired_pipeline"
			return nil
		}
		if observation, err := readAutoPatchObservation(ctx, tx.SQL(), id); err == nil {
			out.Status = observation.Status
			if out.Status == "rolled_back" || out.Status == "insufficient_evidence" {
				out.Reason, out.Status = out.Status, "withdrawn"
			}
			return nil
		} else if !errors.Is(err, harness.ErrNotFound) {
			return err
		}
		if p.Status == "pending_review" {
			disposition, err := s.proposalDispositionTx(ctx, tx.SQL(), id)
			if err != nil {
				return err
			}
			out.Reason = disposition.Reason
		}
		var jobID string
		err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, id).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if p.BehaviorJobID != "" {
			jobID = p.BehaviorJobID
		}
		job, err := skillmaintenance.ReadJob(ctx, tx.SQL(), jobID)
		if err != nil {
			return err
		}
		out.JobID, out.JobState = job.ID, job.State
		out.AutomaticRetry = job.State == "retry"
		if job.State != "completed" && job.ErrorCode != "" {
			out.Reason = job.ErrorCode
		}
		switch out.Reason {
		case "blocked_runtime", "blocked_evidence", "waiting_evidence", "waiting_observation":
			out.Status = out.Reason
		}
		if behavior, err := readBehavior(ctx, tx.SQL(), id); err == nil {
			out.Evidence, err = s.evidenceReadinessTx(ctx, tx.SQL(), *p, behavior)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, harness.ErrNotFound) {
			return err
		}
		return nil
	})
	return
}
