package evolution

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type generationContextKey struct{}

func (s *Service) attachGenerationProposal(ctx context.Context, tx *skills.PublicationTx, p *Proposal) error {
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
	if r.input.PublicationPolicy == harness.SkillPublicationSecurityOnly {
		p.PublicationPolicy, p.GenerationJobID = r.input.PublicationPolicy, id
		return updateProposal(ctx, tx.SQL(), *p, p.StateVersion)
	}
	return s.attachBehaviorGeneration(ctx, tx, *p, r)
}

func (s *Service) guardGenerationValidation(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	if p.PublicationPolicy == "" {
		if behavior, err := readBehavior(ctx, tx.SQL(), p.ID); err == nil && behavior.manual && p.BehaviorJobID == behavior.jobID {
			return s.guardBehaviorManual(ctx, tx, p)
		}
	}
	var id string
	err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if p.PublicationPolicy != "" {
			return harness.ErrPermissionDenied
		}
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
	if r.proposal != p.ID || r.input.PublicationPolicy != p.PublicationPolicy || r.input.Target != p.Target || r.input.Request.Base != p.Base || r.candidate != p.CandidateVersion || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) {
		return harness.ErrSkillConflict
	}
	if p.PublicationPolicy == harness.SkillPublicationSecurityOnly {
		if err := s.generation.checkConfiguration(r); err != nil {
			return err
		}
		if p.GenerationJobID != id || p.Origin != "automatic" || p.OverlayBase != nil || p.Action != "edit" && p.Action != "create" {
			return harness.ErrPermissionDenied
		}
		if r.input.Submission != nil {
			_, err := s.submittedOriginTx(ctx, tx.SQL(), p, r)
			return err
		}
		if r.input.DiscoveryID == "" {
			if p.Trigger.Type != "operator_generation" && p.Trigger.Type != "operator_submission" {
				return harness.ErrPermissionDenied
			}
			return nil
		}
		d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
		if err != nil {
			return err
		}
		if d.kind() != p.Trigger.Type || d.ReviewDraft || s.discovery == nil || d.Options != s.discovery.options {
			return harness.ErrPermissionDenied
		}
		if err := s.guardDiscoveryApproval(ctx, tx, p); err != nil {
			return err
		}
		if d.kind() == "experience_pattern" {
			external, err := s.externallyDerived(ctx, tx, d.Evidence)
			if err != nil {
				return err
			}
			if external {
				return harness.ErrPermissionDenied
			}
		}
		return nil
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
			_ = d // Full packages use the same host validation path.
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
	case errors.Is(err, skillmaintenance.ErrYieldProgress):
		return "yield_progress", false
	case errors.Is(err, skillmaintenance.ErrBlockedEvidence):
		return "blocked_evidence", false
	case errors.Is(err, skillmaintenance.ErrBlockedRuntime):
		return "blocked_runtime", false
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
	case errors.Is(err, skillmaintenance.ErrCapacity), errors.Is(err, skills.ErrCapacity):
		return "deferred_capacity", false
	case errors.Is(err, harness.ErrInvalidInput):
		return "invalid_candidate", false
	case errors.As(err, &review):
		switch review.Code {
		case "review_runtime_unavailable":
			return "blocked_runtime", false
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
		// A submitted or resumed package is already bound to this durable job.
		// Reuse its proposal and exact bytes; do not generate or submit it again.
		return s.finishGeneration(ctx, p)
	}
	if r.candidate == "" {
		candidate, saved, err := s.generatedOutput(ctx, job.ID)
		if err != nil {
			return generationFailure(err)
		}
		if !saved {
			if r.input.GeneratorIdentity != s.generation.identity {
				return generationFailure(errGenerationConfiguration)
			}
			candidate, err = s.generation.generator.Generate(ctx, input)
			if err != nil {
				return generationFailure(err)
			}
			if err = s.persistGeneratedOutput(ctx, job.ID, candidate); err != nil {
				return generationFailure(err)
			}
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
	return s.finishGeneration(ctx, p)
}

func (s *Service) finishGeneration(ctx context.Context, p Proposal) (string, bool) {
	var err error
	if p.Status == "validating" {
		p, err = s.Validate(ctx, p.ID, p.StateVersion)
		if err != nil {
			var failure *harness.SkillValidationFailure
			if p.PublicationPolicy == harness.SkillPublicationSecurityOnly && errors.As(err, &failure) && failure.Code == "review_runtime_unavailable" {
				return "model_failed", true
			}
			return generationFailure(err)
		}
	}
	if p.PublicationPolicy == harness.SkillPublicationSecurityOnly && (p.Status == "pending_review" || p.Status == "publishing") {
		p, err = s.publishSecurityReviewed(ctx, p.ID, p.StateVersion)
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
	rows, err := s.db.QueryContext(ctx, `SELECT i.job_id FROM df_skill_generation_inputs i JOIN df_skill_maintenance_jobs j ON j.id=i.job_id JOIN df_skill_proposals p ON p.id=i.proposal_id WHERE j.state IN ('failed','cancelled') AND (p.status='validating' OR (p.status='pending_review' AND json_extract(p.payload,'$.publication_policy')='security_only' AND NOT EXISTS(SELECT 1 FROM df_skill_approvals a WHERE a.proposal_id=p.id))) AND NOT (j.error_code='configuration_changed' AND EXISTS(SELECT 1 FROM df_skill_behavior_evaluations e WHERE e.proposal_id=p.id AND e.manual=1 AND e.job_id<>i.job_id)) ORDER BY i.job_id LIMIT 32`)
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
			if !generationCanFail(p) {
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

func generationCanFail(p Proposal) bool {
	return p.Status == "validating" || p.Status == "pending_review" && p.PublicationPolicy == harness.SkillPublicationSecurityOnly && p.Approval == nil
}

func closeGenerationProposal(ctx context.Context, tx *sql.Tx, r generationRecord, p Proposal) error {
	if r.job.ErrorCode == "configuration_changed" && p.BehaviorJobID != "" && p.BehaviorJobID != r.job.ID {
		active, err := readBehavior(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if active.manual && active.jobID == p.BehaviorJobID {
			return nil
		}
	}
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
	if r.job.ErrorCode == "base_changed" || p.PublicationPolicy == harness.SkillPublicationSecurityOnly && r.job.ErrorCode == "evidence_changed" {
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
	if !generationCanFail(p) {
		return nil
	}
	return closeGenerationProposal(ctx, tx, r, p)
}

func (s *Service) RunGeneration(ctx context.Context) (bool, error) {
	if s.generation == nil {
		return false, invalid("generation is not configured")
	}
	if err := s.refreshWaiting(ctx); err != nil {
		return false, err
	}
	cleanup, cancel := context.WithTimeout(ctx, 2*time.Second)
	before := s.reconcileGeneration(cleanup)
	if !s.securityPublicationEnabled() {
		before = errors.Join(before, s.reconcileBehavior(cleanup))
	}
	cancel()
	var activeID string
	kinds := []skillmaintenance.JobKind{skillmaintenance.SkillGeneration}
	if !s.securityPublicationEnabled() {
		kinds = append(kinds, skillmaintenance.SkillBehavior)
	}
	for kind := range s.maintenanceProducers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	worked, err := s.generation.store.RunKinds(ctx, kinds, func(work context.Context, job skillmaintenance.Job) (string, bool) {
		activeID = job.ID
		if strings.HasPrefix(job.Request.Reference, "regcheck_") {
			return s.runRegression(work, job)
		}
		if producer, exists := s.maintenanceProducers[job.Request.Kind]; exists {
			return producer.handler(work, job)
		}
		id, err := manualBehaviorProposal(work, s.db, job.ID)
		if err != nil {
			return generationFailure(err)
		}
		if id != "" {
			code, retry := s.reviewBehaviorManual(work, job, id)
			return code, s.boundReviewRetry(work, id, job.Attempts, retry)
		}
		code, retry := s.generateOne(work, job)
		if r, err := readGeneration(work, s.db, job.ID); err == nil && r.proposal != "" {
			retry = s.boundReviewRetry(work, r.proposal, job.Attempts, retry)
		}
		return code, retry
	})
	cleanup, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if errors.Is(err, skillmaintenance.ErrConflict) && activeID != "" {
		if job, readErr := skillmaintenance.ReadJob(cleanup, s.db, activeID); readErr == nil && job.State == "cancelled" {
			err = nil
		}
	}
	if !s.securityPublicationEnabled() {
		err = errors.Join(err, s.reconcileBehavior(cleanup))
	}
	return worked, errors.Join(before, err, s.reconcileGeneration(cleanup))
}

func (s *Service) boundReviewRetry(ctx context.Context, id string, attempts int, retry bool) bool {
	if !retry {
		return false
	}
	p, err := s.Get(ctx, id)
	if err != nil {
		return false
	}
	if p.ValidationFailure != nil && p.ValidationFailure.Code == "review_response_invalid" {
		return attempts < 2
	}
	return retry
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
