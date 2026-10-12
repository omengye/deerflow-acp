package evolution

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type generationContextKey struct{}

func (s *Service) attachGenerationProposal(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	id, _ := ctx.Value(generationContextKey{}).(string)
	if id == "" {
		return nil
	}
	if err := skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
		return err
	}
	r, err := readGeneration(ctx, tx.SQL(), id)
	if err != nil {
		return err
	}
	if r.input.Target != p.Target || r.input.Request.Base != p.Base || r.candidate == "" || r.candidate != p.CandidateVersion || r.proposal != "" && r.proposal != p.ID || p.Origin != "automatic" || p.Author != r.input.Request.Author || p.Reason != r.reason || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) {
		return harness.ErrSkillConflict
	}
	if _, err = s.generation.discoveryEvidence(ctx, tx.SQL(), r); err != nil {
		return err
	}
	r.proposal = p.ID
	_, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_generation_inputs SET proposal_id=?,progress_sha=? WHERE job_id=?`, p.ID, generationProgress(r), id)
	if err != nil {
		return err
	}
	return s.attachBehaviorGeneration(ctx, tx, p, r)
}

func (s *Service) guardGenerationValidation(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	var id string
	err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return s.guardBehaviorManual(ctx, tx, p)
	}
	if err != nil {
		return err
	}
	want, _ := ctx.Value(generationContextKey{}).(string)
	if want != id {
		return harness.ErrPermissionDenied
	}
	if err = skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
		return err
	}
	r, err := readGeneration(ctx, tx.SQL(), id)
	if err != nil {
		return err
	}
	if r.input.Target != p.Target || r.input.Request.Base != p.Base || r.candidate != p.CandidateVersion || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) {
		return harness.ErrSkillConflict
	}
	if err = s.guardExperienceCandidate(ctx, tx, r); err != nil {
		return err
	}
	_, err = s.generation.discoveryEvidence(ctx, tx.SQL(), r)
	return err
}

func (s *Service) generationInput(ctx context.Context, id string) (generationRecord, skillmaintenance.GenerationInput, error) {
	var result generationRecord
	var input skillmaintenance.GenerationInput
	err := s.withGeneration(ctx, id, func(tx *skills.PublicationTx, r generationRecord) error {
		if err := skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
			return err
		}
		if err := s.generation.checkSource(r); err != nil {
			return err
		}
		if r.proposal != "" {
			p, err := readProposal(ctx, tx.SQL(), r.proposal)
			if err != nil {
				return err
			}
			if p.Target != r.input.Target || p.Base != r.input.Request.Base || p.CandidateVersion != r.candidate {
				return harness.ErrSkillConflict
			}
			// A committed review may already have been approved while a worker
			// was interrupted. Acknowledge that durable result without using a
			// new model configuration or requiring the former installed head.
			if p.Status != "validating" {
				if p.Status == "pending_review" {
					if err := s.guardExperienceCandidate(ctx, tx, r); err != nil {
						return err
					}
				}
				result = r
				return nil
			}
		}
		if err := s.guardExperienceCandidate(ctx, tx, r); err != nil {
			return err
		}
		if err := s.generation.checkConfiguration(r); err != nil {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != r.input.Request.Base {
			return harness.ErrSkillConflict
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if !skills.SameOverlayBase(overlay, r.input.OverlayBase) {
			return harness.ErrSkillConflict
		}
		evidence, err := s.generation.discoveryEvidence(ctx, tx.SQL(), r)
		if err != nil {
			return err
		}
		input = skillmaintenance.GenerationInput{Target: r.input.Target, Base: r.input.Request.Base, Reason: r.input.Request.Reason, Evidence: evidence}
		if r.input.DiscoveryID != "" {
			d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
			if err != nil {
				return err
			}
			input.InstructionOnly = d.kind() == "experience_pattern"
		}
		if input.Base.Exists {
			base, err := tx.ReadVersion(input.Base.Version)
			if err != nil {
				return err
			}
			input.BaseFiles = base.Files()
		}
		result = r
		return nil
	})
	return result, input, err
}

func (s *Service) saveGeneratedCandidate(ctx context.Context, id string, c skillmaintenance.Candidate) error {
	if c.Reason == "" || !bounded(c.Reason, 4000) {
		return invalid("generated reason exceeds bounds")
	}
	if err := validateFiles(c.Files); err != nil {
		return err
	}
	r, err := readGeneration(ctx, s.db, id)
	if err != nil {
		return err
	}
	candidate, err := s.registry.PrepareCandidate(r.input.Target.Name, c.Files)
	if err != nil {
		return err
	}
	return s.withGeneration(ctx, id, func(tx *skills.PublicationTx, r generationRecord) error {
		if err := skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
			return err
		}
		if err := s.generation.checkConfiguration(r); err != nil {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != r.input.Request.Base {
			return harness.ErrSkillConflict
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if !skills.SameOverlayBase(overlay, r.input.OverlayBase) {
			return harness.ErrSkillConflict
		}
		if _, err := s.generation.discoveryEvidence(ctx, tx.SQL(), r); err != nil {
			return err
		}
		if r.candidate != "" {
			return harness.ErrSkillConflict
		}
		if candidate.Version() == r.input.Request.Base.Version {
			return invalid("generated candidate has no changes")
		}
		experience := false
		if r.input.DiscoveryID != "" {
			d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
			if err != nil {
				return err
			}
			if d.kind() == "experience_pattern" {
				experience = true
				if err := validateCandidateCredentials(candidate, true); err != nil {
					return err
				}
				if head.Exists {
					base, err := tx.ReadVersion(head.Version)
					if err != nil {
						return err
					}
					files := base.Files()
					if len(c.Files) != len(files) {
						return invalid("experience candidates cannot add or remove support files")
					}
					for path, data := range files {
						other, exists := c.Files[path]
						if !exists || path != "SKILL.md" && !bytes.Equal(data, other) {
							return invalid("experience candidates can only change SKILL.md")
						}
					}
				} else if len(c.Files) != 1 || c.Files["SKILL.md"] == nil {
					return invalid("new experience skills may only contain SKILL.md")
				}
				c.Reason, err = learning.RedactPromotionReason(c.Reason)
				if err != nil {
					return err
				}
				var retained bool
				if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_versions WHERE source_key=? AND name=? AND version=?)`, r.input.Target.SourceIdentity, r.input.Target.Name, candidate.Version()).Scan(&retained); err != nil {
					return err
				}
				if !retained {
					if _, err := tx.SQL().ExecContext(ctx, `INSERT OR IGNORE INTO df_learning_owned_versions(source_key,name,version) VALUES(?,?,?)`, r.input.Target.SourceIdentity, r.input.Target.Name, candidate.Version()); err != nil {
						return err
					}
				}
			}
		}
		if !experience {
			if err := validateCandidateCredentials(candidate, false); err != nil {
				return err
			}
		}
		r.candidate, err = tx.Stage(candidate)
		if err != nil {
			return err
		}
		r.reason = c.Reason
		_, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_generation_inputs SET candidate_version=?,candidate_reason=?,progress_sha=? WHERE job_id=?`, r.candidate, r.reason, generationProgress(r), id)
		return err
	})
}

func generationFailure(err error) (string, bool) {
	var review *harness.SkillValidationFailure
	switch {
	case errors.Is(err, errGenerationConfiguration):
		return "configuration_changed", false
	case errors.Is(err, skillmaintenance.ErrSourceActive):
		return "source_active", true
	case errors.Is(err, errDiscoveryEvidence):
		return "evidence_changed", false
	case errors.Is(err, harness.ErrPermissionDenied):
		return "source_unavailable", false
	case errors.Is(err, harness.ErrSkillConflict):
		return "base_changed", false
	case errors.Is(err, skillmaintenance.ErrBudget):
		return "budget_exhausted", false
	case errors.Is(err, harness.ErrInvalidInput):
		return "invalid_candidate", false
	case errors.As(err, &review):
		switch review.Code {
		case "review_budget_exhausted":
			return "budget_exhausted", false
		case "review_storage_error":
			return "storage_error", false
		case "review_cancelled":
			return "cancelled", false
		default:
			return "model_failed", review.Retryable
		}
	case errors.Is(err, harness.ErrSkillValidation):
		return "model_failed", true
	default:
		return "storage_error", true
	}
}

func (s *Service) generateOne(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	ctx = context.WithValue(ctx, generationContextKey{}, job.ID)
	r, input, err := s.generationInput(ctx, job.ID)
	if err != nil {
		return generationFailure(err)
	}
	if r.proposal != "" {
		p, err := s.Get(ctx, r.proposal)
		if err != nil {
			return generationFailure(err)
		}
		if p.Status != "validating" {
			return generationProposalOutcome(p)
		}
	}
	if r.candidate == "" {
		candidate, err := s.generation.generator.Generate(ctx, input)
		if err != nil {
			return generationFailure(err)
		}
		if err = s.saveGeneratedCandidate(ctx, job.ID, candidate); err != nil {
			return generationFailure(err)
		}
	}
	var request Request
	err = s.withGeneration(ctx, job.ID, func(tx *skills.PublicationTx, saved generationRecord) error {
		if err := skillmaintenance.GuardTx(ctx, tx.SQL(), job.ID); err != nil {
			return err
		}
		r = saved
		candidate, err := tx.ReadVersion(r.candidate)
		if err != nil {
			return err
		}
		action := "create"
		if r.input.Request.Base.Exists {
			action = "edit"
		}
		request = Request{SourceID: r.input.Target.SourceID, Name: r.input.Target.Name, RequestKey: "generation/" + job.ID, Action: action, Reason: r.reason, Author: r.input.Request.Author, Origin: "automatic", Base: r.input.Request.Base, Files: candidate.Files(), Trigger: Trigger{Type: "operator_generation", Summary: r.input.Request.Reason}}
		if r.input.DiscoveryID != "" {
			d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
			if err != nil {
				return err
			}
			request.Trigger = Trigger{Type: d.kind(), Summary: "Discovery " + r.input.DiscoveryID + "; evidence references revalidated; causality unassessed."}
		}
		return nil
	})
	if err != nil {
		return generationFailure(err)
	}
	p, err := s.submit(ctx, request, false)
	if err != nil {
		return generationFailure(err)
	}
	if p.Status == "validating" {
		p, err = s.Validate(ctx, p.ID, p.StateVersion)
		if err != nil {
			return generationFailure(err)
		}
	}
	return generationProposalOutcome(p)
}

func generationProposalOutcome(p Proposal) (string, bool) {
	switch p.Status {
	case "pending_review", "published", "rejected":
		return "", false
	case "stale":
		return "base_changed", false
	default:
		return "invalid_candidate", false
	}
}

// Failed/interrupted workers may leave a validating proposal, but cannot make
// it reviewable. Bounded, replayable metadata cleanup closes that record on
// failure/cancellation, including after a process crash during finalization.
func (s *Service) reconcileGeneration(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT i.job_id FROM df_skill_generation_inputs i JOIN df_skill_maintenance_jobs j ON j.id=i.job_id JOIN df_skill_proposals p ON p.id=i.proposal_id WHERE j.state IN ('failed','cancelled') AND p.status='validating' ORDER BY i.job_id LIMIT 32`)
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
		err = func() error {
			r, err := readGeneration(ctx, tx, id)
			if err != nil {
				return err
			}
			if r.job.State != "failed" && r.job.State != "cancelled" {
				return nil
			}
			p, err := readProposal(ctx, tx, r.proposal)
			if err != nil {
				return err
			}
			if p.Status != "validating" {
				return nil
			}
			return closeGenerationProposal(ctx, tx, r, p)
		}()
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

func closeGenerationProposal(ctx context.Context, tx *sql.Tx, r generationRecord, p Proposal) error {
	if p.Target != r.input.Target || p.Base != r.input.Request.Base || p.CandidateVersion != r.candidate {
		return harness.ErrSkillConflict
	}
	previous := p.StateVersion
	p.StateVersion++
	p.UpdatedAt = time.Now().UTC()
	p.Status = "failed"
	p.Error = "Skill generation stopped before review completed."
	if p.ValidationFailure != nil {
		p.ValidationFailure.Retryable = false
		p.Error = p.ValidationFailure.Error()
	}
	if r.job.State == "cancelled" {
		p.Status = "rejected"
		p.Error = "Skill generation was cancelled by the operator."
	}
	if r.job.ErrorCode == "base_changed" {
		p.Status = "stale"
	}
	if p.EvaluationDigest == "" {
		p.Evaluation = Report{BaseVersion: p.Base.Version, CandidateVersion: p.CandidateVersion, Security: "unavailable", Behavior: "not_run"}
	}
	if behavior, err := readBehavior(ctx, tx, p.ID); err == nil {
		if behavior.content != nil {
			p.Evaluation = *behavior.content
		}
		if behavior.input.Suite != nil {
			p.Evaluation.Behavior = "unavailable"
		}
		p.Evaluation.BehaviorReport = nil
		p.Evaluation.BehaviorDigest = ""
	} else if !errors.Is(err, harness.ErrNotFound) {
		return err
	}
	p.EvaluationDigest = encodedHash(p.Evaluation)
	return updateProposal(ctx, tx, p, previous)
}

func finalizeGenerationTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	if job.State != "failed" && job.State != "cancelled" {
		return nil
	}
	r, err := readGeneration(ctx, tx, job.ID)
	if errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrInvalidInput) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.proposal == "" {
		return nil
	}
	p, err := readProposal(ctx, tx, r.proposal)
	if err != nil {
		return err
	}
	if p.Status != "validating" {
		return nil
	}
	return closeGenerationProposal(ctx, tx, r, p)
}

func (s *Service) RunGeneration(ctx context.Context) (bool, error) {
	if s.generation == nil {
		return false, invalid("generation is not configured")
	}
	cleanup, cancel := context.WithTimeout(ctx, 2*time.Second)
	before := s.reconcileGeneration(cleanup)
	before = errors.Join(before, s.reconcileBehavior(cleanup))
	cancel()
	var activeID string
	kinds := []skillmaintenance.JobKind{skillmaintenance.SkillGeneration, skillmaintenance.SkillBehavior}
	for kind := range s.maintenanceProducers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	worked, err := s.generation.store.RunKinds(ctx, kinds, func(work context.Context, job skillmaintenance.Job) (string, bool) {
		activeID = job.ID
		if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists {
			return producer.handler(work, job)
		}
		id, err := manualBehaviorProposal(work, s.db, job.ID)
		if err != nil {
			return generationFailure(err)
		}
		if id != "" {
			return s.reviewBehaviorManual(work, job, id)
		}
		return s.generateOne(work, job)
	})
	cleanup, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if errors.Is(err, skillmaintenance.ErrConflict) && activeID != "" {
		if job, readErr := skillmaintenance.ReadJob(cleanup, s.db, activeID); readErr == nil && job.State == "cancelled" {
			err = nil
		}
	}
	return worked, errors.Join(before, err, s.reconcileGeneration(cleanup), s.reconcileBehavior(cleanup))
}

func (s *Service) StartGeneration() error {
	g := s.generation
	if g == nil {
		return nil
	}
	g.lifecycle.mu.Lock()
	defer g.lifecycle.mu.Unlock()
	if g.lifecycle.closed {
		return invalid("generation worker is closed")
	}
	if g.lifecycle.done != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.lifecycle.cancel = cancel
	g.lifecycle.done = make(chan struct{})
	var workers sync.WaitGroup
	for range g.workers {
		workers.Go(func() {
			warned := false
			for {
				worked, err := s.RunGeneration(ctx)
				if ctx.Err() != nil {
					return
				}
				delay := time.Second
				if err != nil {
					if !warned {
						slog.Warn("skill generation maintenance failed; user task state is unchanged")
					}
					warned = true
					delay = 5 * time.Second
				} else {
					warned = false
					if worked {
						delay = 10 * time.Millisecond
					}
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}
	go func() { workers.Wait(); close(g.lifecycle.done) }()
	return nil
}

func (s *Service) closeGeneration() {
	g := s.generation
	if g == nil {
		return
	}
	g.lifecycle.mu.Lock()
	g.lifecycle.closed = true
	if g.lifecycle.cancel != nil {
		g.lifecycle.cancel()
	}
	done := g.lifecycle.done
	g.lifecycle.mu.Unlock()
	if done != nil {
		<-done
	}
}
