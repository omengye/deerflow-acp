package evolution

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (s *Service) BehaviorEvaluation(ctx context.Context, proposalID string) (harness.SkillBehaviorEvaluation, error) {
	var view harness.SkillBehaviorEvaluation
	err := s.withProposal(ctx, proposalID, func(tx *skills.PublicationTx, p *Proposal) error {
		r, err := readBehavior(ctx, tx.SQL(), p.ID)
		if err != nil {
			return err
		}
		if r.input.Target != p.Target || r.input.Base != p.Base || r.input.CandidateVersion != p.CandidateVersion || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) {
			return harness.ErrSkillConflict
		}
		job, err := skillmaintenance.ReadJob(ctx, tx.SQL(), r.jobID)
		if err != nil {
			return err
		}
		results, err := readBehaviorResults(ctx, tx.SQL(), r)
		if err != nil {
			return err
		}
		if r.report != nil {
			composed, err := composeBehaviorReport(r, results)
			if err != nil {
				return err
			}
			if encodedHash(composed) != encodedHash(*r.report) {
				return invalid("behavior report does not match persisted cases")
			}
		}
		view = harness.SkillBehaviorEvaluation{ID: r.input.ID, ProposalID: p.ID, JobID: r.jobID, Target: r.input.Target, Base: r.input.Base, CandidateVersion: r.input.CandidateVersion, SuiteDigest: r.input.SuiteDigest, EvaluatorIdentity: r.input.EvaluatorIdentity, State: job.State, StateVersion: job.StateVersion, Attempts: job.Attempts, ErrorCode: job.ErrorCode, Deadline: job.Deadline, RetryAt: job.RetryAt, ContentReviewed: r.content != nil, Results: results, Report: r.report}
		if r.input.Suite != nil {
			view.SuiteID = r.input.Suite.ID
		}
		return nil
	})
	return view, err
}

func (s *Service) BehaviorReceipts(ctx context.Context, proposalID string, after int64, limit int) ([]harness.SkillMaintenanceReceipt, error) {
	var result []harness.SkillMaintenanceReceipt
	err := s.withProposal(ctx, proposalID, func(tx *skills.PublicationTx, p *Proposal) error {
		r, err := readBehavior(ctx, tx.SQL(), p.ID)
		if err != nil {
			return err
		}
		if r.input.Target != p.Target {
			return harness.ErrPermissionDenied
		}
		result, err = skillmaintenance.ReadReceipts(ctx, tx.SQL(), r.jobID, after, limit)
		return err
	})
	return result, err
}
