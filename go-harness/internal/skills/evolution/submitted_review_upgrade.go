package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// Requeue live packages for a fresh safety decision. Retain source bindings,
// measurements and accounting from the previous policy.
func (s *Service) upgradeSecurityReviews(ctx context.Context) error {
	ids, err := s.securityUpgradeIDs(ctx, `SELECT p.id FROM df_skill_proposals p
 JOIN df_skill_generation_inputs g ON g.proposal_id=p.id
 JOIN df_skill_maintenance_jobs j ON j.id=COALESCE(NULLIF(json_extract(p.payload,'$.behavior_job_id'),''),g.job_id)
 WHERE p.status IN ('validating','pending_review') AND json_extract(p.payload,'$.origin')='automatic'
 AND (json_extract(p.payload,'$.trigger.type')='explicit_agent_proposal' OR (? AND json_extract(g.payload,'$.workspace') IS NULL)) AND json_extract(p.payload,'$.publication_policy') IS NULL
 AND j.state IN ('queued','running','retry','waiting','completed')
 AND NOT EXISTS(SELECT 1 FROM df_evolution_retired_proposals r WHERE r.proposal_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM df_skill_approvals a WHERE a.proposal_id=p.id)`, s.securityPublicationEnabled())
	if err != nil {
		return err
	}
	for _, id := range ids {
		err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
			return s.upgradeReviewTx(ctx, tx, p)
		})
		if err != nil && !unavailableEvidence(err) {
			return err
		}
	}
	if !s.securityPublicationEnabled() {
		return nil
	}
	if err = s.upgradeQueuedGeneration(ctx); err != nil {
		return err
	}
	return s.finishLegacyObservations(ctx)
}

func (s *Service) upgradeQueuedGeneration(ctx context.Context) error {
	ids, err := s.securityUpgradeIDs(ctx, `SELECT i.job_id FROM df_skill_generation_inputs i JOIN df_skill_maintenance_jobs j ON j.id=i.job_id
 WHERE i.proposal_id='' AND json_extract(i.payload,'$.publication_policy') IS NULL AND j.state IN ('queued','running','retry','waiting')`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		err = s.withGeneration(ctx, id, func(tx *skills.PublicationTx, r generationRecord) error {
			if r.proposal != "" || r.input.PublicationPolicy != "" || r.job.State != "queued" && r.job.State != "running" && r.job.State != "retry" && r.job.State != "waiting" {
				return nil
			}
			r.input.PublicationPolicy = harness.SkillPublicationSecurityOnly
			r.input.ValidatorIdentity, r.input.BehaviorIdentity = s.generation.reviewIdentity, ""
			if r.candidate == "" {
				var saved bool
				if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_generation_outputs WHERE job_id=?)`, r.job.ID).Scan(&saved); err != nil {
					return err
				}
				if !saved {
					r.input.GeneratorIdentity = s.generation.identity
				}
			}
			return requeueSecurityReview(ctx, tx.SQL(), r)
		})
		if err != nil && !unavailableEvidence(err) {
			return err
		}
	}
	// Fence standalone historical behavior evaluations, including regression
	// jobs, before starting workers. Keep their records and usage ledger.
	_, err = s.db.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled' WHERE job_kind='skill_behavior' AND state IN ('queued','retry','running','waiting')`)
	return err
}

func (s *Service) upgradeReviewTx(ctx context.Context, tx *skills.PublicationTx, p *Proposal) error {
	if p.PublicationPolicy != "" || p.Approval != nil || (p.Status != "validating" && p.Status != "pending_review") {
		return nil
	}
	var jobID string
	if err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&jobID); err != nil {
		return err
	}
	r, err := readGeneration(ctx, tx.SQL(), jobID)
	if err != nil {
		return err
	}
	if r.input.PublicationPolicy != "" || r.input.Target != p.Target || r.input.Request.Base != p.Base || r.candidate != p.CandidateVersion {
		return harness.ErrPermissionDenied
	}
	o, err := s.submittedOriginTx(ctx, tx.SQL(), *p, r)
	if err != nil {
		return err
	}
	if o == nil && (!s.securityPublicationEnabled() || r.input.Workspace != "" || p.Trigger.Type != "operator_generation" && !isDiscoveryTrigger(p.Trigger.Type)) {
		return harness.ErrPermissionDenied
	}
	r.input.Submission, r.input.PublicationPolicy = o, harness.SkillPublicationSecurityOnly
	r.input.ValidatorIdentity, r.input.BehaviorIdentity = s.generation.reviewIdentity, ""
	old, err := readBehavior(ctx, tx.SQL(), p.ID)
	if err != nil && !errors.Is(err, harness.ErrNotFound) {
		return err
	}
	if err == nil {
		if old.input.Submission != nil && (o == nil || *old.input.Submission != *o) {
			return errDiscoveryEvidence
		}
		if old.jobID != jobID {
			job, err := skillmaintenance.ReadJob(ctx, tx.SQL(), old.jobID)
			if err != nil {
				return err
			}
			switch job.State {
			case "queued", "running", "retry", "waiting":
				if err = skillmaintenance.CancelTx(ctx, tx.SQL(), job.ID, job.StateVersion); err != nil {
					return err
				}
			case "completed":
			default:
				return nil
			}
		} else if r.job.State == "cancelled" || r.job.State == "failed" {
			return nil
		}
		if err = archiveBehavior(ctx, tx.SQL(), old); err != nil {
			return err
		}
	}
	if err = requeueSecurityReview(ctx, tx.SQL(), r); err != nil {
		return err
	}
	previous := p.StateVersion
	p.StateVersion++
	p.PublicationPolicy, p.Status = r.input.PublicationPolicy, "validating"
	p.GenerationJobID, p.BehaviorJobID = jobID, ""
	p.Evaluation, p.EvaluationDigest, p.ValidationFailure = Report{}, "", nil
	p.ScriptReviewRequired, p.Error, p.UpdatedAt = false, "", time.Now().UTC()
	return updateProposal(ctx, tx.SQL(), *p, previous)
}

func requeueSecurityReview(ctx context.Context, tx *sql.Tx, r generationRecord) error {
	raw, err := json.Marshal(r.input)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_generation_inputs SET payload=? WHERE job_id=?`, raw, r.job.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET input_digest=?,state='queued',state_version=state_version+1,
 token='',attempts=0,lease_ns=0,deadline_ns=0,retry_ns=0,error_code='' WHERE id=?`, hash(raw), r.job.ID); err != nil {
		return err
	}
	return nil
}

func (s *Service) securityUpgradeIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return ids, nil
}
