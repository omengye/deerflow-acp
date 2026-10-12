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

func manualBehaviorProposal(ctx context.Context, db rowReader, jobID string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT proposal_id FROM df_skill_behavior_evaluations WHERE job_id=? AND manual=1`, jobID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Service) admitMaintenance(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists {
		return producer.hooks.Admission(ctx, tx, job)
	}
	if job.Request.Kind != skillmaintenance.SkillGeneration && job.Request.Kind != skillmaintenance.SkillBehavior {
		return harness.ErrPermissionDenied
	}
	id, err := manualBehaviorProposal(ctx, tx, job.ID)
	if err != nil {
		return err
	}
	if id == "" {
		return s.generation.admit(ctx, tx, job)
	}
	r, err := readBehavior(ctx, tx, id)
	if err != nil {
		return err
	}
	p, err := readProposal(ctx, tx, id)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(r.input)
	if err != nil {
		return err
	}
	if !r.manual || r.jobID != job.ID || r.input.ID != job.Request.Reference || hash(payload) != job.Request.InputDigest || job.Request.SourceIdentity != p.Target.SourceIdentity || job.Request.Name != p.Target.Name || p.BehaviorJobID != job.ID {
		return harness.ErrSkillConflict
	}
	return s.checkBehavior(ctx, tx, r, p)
}

func (s *Service) guardBehaviorManual(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	r, err := readBehavior(ctx, tx.SQL(), p.ID)
	if errors.Is(err, harness.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !r.manual {
		return harness.ErrSkillConflict
	}
	id, _ := ctx.Value(generationContextKey{}).(string)
	if id != r.jobID {
		return harness.ErrPermissionDenied
	}
	if err = skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
		return err
	}
	job, err := skillmaintenance.ReadJob(ctx, tx.SQL(), id)
	if err != nil {
		return err
	}
	if err = s.admitMaintenance(ctx, tx.SQL(), job); err != nil {
		return err
	}
	overlay, err := tx.OverlayBase()
	if err != nil {
		return err
	}
	if !skills.SameOverlayBase(overlay, r.input.OverlayBase) {
		return harness.ErrSkillConflict
	}
	return nil
}

func (s *Service) reviewBehaviorManual(ctx context.Context, job skillmaintenance.Job, id string) (string, bool) {
	ctx = context.WithValue(ctx, generationContextKey{}, job.ID)
	p, err := s.Get(ctx, id)
	if err != nil {
		return generationFailure(err)
	}
	if p.Status != "validating" {
		return generationProposalOutcome(p)
	}
	p, err = s.Validate(ctx, p.ID, p.StateVersion)
	if err != nil {
		return generationFailure(err)
	}
	return generationProposalOutcome(p)
}

func (s *Service) finalizeMaintenanceTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists {
		return producer.hooks.Terminal(ctx, tx, job)
	}
	if job.Request.Kind != skillmaintenance.SkillGeneration && job.Request.Kind != skillmaintenance.SkillBehavior {
		return harness.ErrPermissionDenied
	}
	id, err := manualBehaviorProposal(ctx, tx, job.ID)
	if err != nil {
		return err
	}
	if id == "" {
		return finalizeGenerationTx(ctx, tx, job)
	}
	if job.State != "failed" && job.State != "cancelled" {
		return nil
	}
	r, err := readBehavior(ctx, tx, id)
	if err != nil {
		return err
	}
	p, err := readProposal(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.Status != "validating" {
		return nil
	}
	previous := p.StateVersion
	p.StateVersion++
	p.UpdatedAt = time.Now().UTC()
	p.Status = "failed"
	p.Error = "Skill evaluation stopped before review completed."
	if p.ValidationFailure != nil {
		p.ValidationFailure.Retryable = false
		p.Error = p.ValidationFailure.Error()
	}
	if job.State == "cancelled" {
		p.Status = "rejected"
		p.Error = "Skill evaluation was cancelled by the operator."
	}
	if job.ErrorCode == "base_changed" || job.ErrorCode == "evidence_changed" {
		p.Status = "stale"
	}
	if p.EvaluationDigest == "" {
		p.Evaluation = Report{BaseVersion: p.Base.Version, CandidateVersion: p.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
	}
	if r.content != nil {
		p.Evaluation = *r.content
	}
	if r.input.Suite != nil {
		p.Evaluation.Behavior = "unavailable"
	}
	p.Evaluation.BehaviorReport = nil
	p.Evaluation.BehaviorDigest = ""
	p.EvaluationDigest = encodedHash(p.Evaluation)
	return updateProposal(ctx, tx, p, previous)
}

func (s *Service) cancelBehaviorManual(ctx context.Context, tx *sql.Tx, p Proposal) error {
	r, err := readBehavior(ctx, tx, p.ID)
	if errors.Is(err, harness.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, r.jobID)
	if err != nil {
		return err
	}
	if job.State != "queued" && job.State != "running" && job.State != "retry" {
		return nil
	}
	return skillmaintenance.CancelTx(ctx, tx, job.ID, job.StateVersion)
}

func (s *Service) reconcileBehavior(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT e.job_id FROM df_skill_behavior_evaluations e JOIN df_skill_maintenance_jobs j ON j.id=e.job_id JOIN df_skill_proposals p ON p.id=e.proposal_id WHERE e.manual=1 AND j.state IN ('failed','cancelled') AND p.status='validating' ORDER BY e.job_id LIMIT 32`)
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
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		job, err := skillmaintenance.ReadJob(ctx, tx, id)
		if err == nil {
			err = s.finalizeMaintenanceTx(ctx, tx, job)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
