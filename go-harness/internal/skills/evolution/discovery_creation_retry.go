package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (s *Service) PrepareNewSkillRetry(ctx context.Context, actor string, request harness.SkillLearningNewSkillRetryPreparationRequest) (harness.SkillLearningNewSkillRetryPreparation, error) {
	var result harness.SkillLearningNewSkillRetryPreparation
	if s.experienceSource == nil {
		return result, harness.ErrPermissionDenied
	}
	err := s.withGeneration(ctx, request.PreviousGenerationID, func(pub *skills.PublicationTx, previous generationRecord) error {
		var err error
		result, _, _, err = s.prepareCreationRetryTx(ctx, pub, actor, request, previous)
		return err
	})
	return result, err
}

func retryScopeRequest(r harness.SkillLearningNewSkillRetryRequest) harness.SkillLearningNewSkillRetryPreparationRequest {
	return harness.SkillLearningNewSkillRetryPreparationRequest{PatternID: r.PatternID, ExpectedPatternRevision: r.ExpectedPatternRevision,
		ExpectedStateVersion: r.ExpectedStateVersion, Scope: r.Scope, PreviousGenerationID: r.PreviousGenerationID}
}

func creationRetryTerminalTx(ctx context.Context, tx *sql.Tx, r generationRecord) (string, int64, error) {
	if r.job.State != "failed" && r.job.State != "cancelled" && r.job.State != "completed" {
		return "", 0, harness.ErrSkillConflict
	}
	if r.proposal == "" {
		if r.job.State == "completed" {
			return "", 0, harness.ErrSkillConflict
		}
		return "", 0, nil
	}
	p, err := readProposal(ctx, tx, r.proposal)
	if err != nil {
		return "", 0, err
	}
	if p.Target != r.input.Target || p.Base != r.input.Request.Base || p.CandidateVersion != r.candidate ||
		p.Status != "rejected" && p.Status != "failed" {
		return "", 0, harness.ErrSkillConflict
	}
	var adopted bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_publications WHERE json_extract(CAST(payload AS TEXT),'$.source_proposal_id')=?)`, p.ID).Scan(&adopted); err != nil {
		return "", 0, err
	}
	if adopted {
		return "", 0, harness.ErrSkillConflict
	}
	return p.Status, p.StateVersion, nil
}

// The original episode reservation is shared by authorized attempts. This is
// provenance reuse, not a second vote; no reservation is deleted or reassigned.
func creationRetryOriginalTx(ctx context.Context, tx *sql.Tx, d discoverySnapshot) (discoverySnapshot, string, error) {
	if d.CreationRetry == nil {
		_, job, _, err := readDiscovery(ctx, tx, d.ID)
		return d, job, err
	}
	original, job, digest, err := readDiscovery(ctx, tx, d.CreationRetry.EvidenceReservationID)
	if err != nil {
		return original, "", err
	}
	r, err := readGeneration(ctx, tx, job)
	if err != nil {
		return original, "", err
	}
	if original.CreationRetry != nil || original.kind() != "experience_pattern" || original.Base != (harness.SkillHead{}) ||
		original.Target != d.Target || original.Options != d.Options || original.HostAdjudicated ||
		r.input.DiscoveryID != original.ID || r.input.DiscoveryDigest != digest || r.input.Target != original.Target || r.input.Request.Base != original.Base ||
		encodedHash(original.Evidence) != encodedHash(d.Evidence) {
		return original, "", errDiscoveryEvidence
	}
	previous, err := readGeneration(ctx, tx, d.CreationRetry.PreviousGenerationID)
	if err != nil {
		return original, "", err
	}
	if _, _, err = creationRetryTerminalTx(ctx, tx, previous); err != nil {
		return original, "", err
	}
	prior, priorJob, priorSHA, err := readDiscovery(ctx, tx, previous.input.DiscoveryID)
	if err != nil {
		return original, "", err
	}
	if priorJob != previous.job.ID || previous.input.DiscoveryDigest != priorSHA || previous.input.Target != d.Target ||
		prior.Target != d.Target || encodedHash(prior.Evidence) != encodedHash(original.Evidence) || prior.Options != d.Options ||
		(d.CreationRetry.RetryNumber == 1 && (prior.ID != original.ID || prior.CreationRetry != nil)) ||
		(d.CreationRetry.RetryNumber > 1 && (prior.CreationRetry == nil || prior.CreationRetry.EvidenceReservationID != original.ID || prior.CreationRetry.RetryNumber+1 != d.CreationRetry.RetryNumber)) {
		return original, "", errDiscoveryEvidence
	}
	return original, job, nil
}

func (s *Service) prepareCreationRetryTx(ctx context.Context, pub *skills.PublicationTx, actor string, request harness.SkillLearningNewSkillRetryPreparationRequest, previous generationRecord) (harness.SkillLearningNewSkillRetryPreparation, discoverySnapshot, string, error) {
	var result harness.SkillLearningNewSkillRetryPreparation
	var d discoverySnapshot
	if s.generation == nil || s.discovery == nil || s.experienceSource == nil || !s.discovery.options.ExperienceNewSkillsEnabled || !s.discovery.options.ExperiencePatternsEnabled {
		return result, d, "", harness.ErrPermissionDenied
	}
	tx := pub.SQL()
	if _, err := s.experienceSource.NewSkillRetryScopeTx(ctx, tx, actor, request); err != nil {
		return result, d, "", err
	}
	var err error
	d, job, digest, err := readDiscovery(ctx, tx, previous.input.DiscoveryID)
	if err != nil {
		return result, d, "", err
	}
	if d.kind() != "experience_pattern" || d.HostAdjudicated || d.ReviewDraft || d.Base != (harness.SkillHead{}) || d.OverlayBase != nil ||
		previous.input.DiscoveryDigest != digest || job != request.PreviousGenerationID || previous.input.Request.Base != d.Base ||
		pub.Target() != d.Target || previous.input.Target != d.Target || d.Options != s.discovery.options ||
		d.Evidence[0].Experience.PatternID != request.PatternID || d.Evidence[0].Experience.PatternRevision != request.ExpectedPatternRevision {
		return result, d, "", harness.ErrSkillConflict
	}
	status, proposalVersion, err := creationRetryTerminalTx(ctx, tx, previous)
	if err != nil {
		return result, d, "", err
	}
	original, automatic, err := creationRetryOriginalTx(ctx, tx, d)
	if err != nil {
		return result, d, "", err
	}
	if d.CreationRetry != nil {
		binding, err := s.experienceSource.NewSkillRetryRecordTx(ctx, tx, request.PatternID, previous.job.ID)
		if err != nil {
			return result, d, "", err
		}
		if binding.DiscoveryID != d.ID || binding.DiscoveryDigest != digest || binding.Reference != *d.CreationRetry || binding.AutomaticGenerationID != automatic || previous.input.CreationRetryAuthorization != binding.AuthorizationDigest {
			return result, d, "", errDiscoveryEvidence
		}
	}
	if err = s.experienceSource.GuardNewPromotionTx(ctx, tx, request.PatternID, automatic); err != nil {
		return result, d, "", err
	}
	if err = s.experienceSource.GuardPromotionTx(ctx, tx, previous.job.ID, d.Evidence[0].Experience.MatchID, request.PatternID); err != nil {
		return result, d, "", err
	}
	ordinal, err := s.experienceSource.NewSkillRetryOrdinalTx(ctx, tx, request.PatternID, previous.job.ID)
	if err != nil {
		return result, d, "", err
	}
	if d.CreationRetry == nil && ordinal != 1 || d.CreationRetry != nil && d.CreationRetry.RetryNumber+1 != ordinal {
		return result, d, "", harness.ErrSkillConflict
	}
	head, err := pub.Current()
	if err != nil {
		return result, d, "", err
	}
	overlay, err := pub.OverlayBase()
	if err != nil {
		return result, d, "", err
	}
	if head != (harness.SkillHead{}) || overlay != nil || s.generation.checkSource(previous) != nil {
		return result, d, "", harness.ErrSkillConflict
	}
	if err = guardNewExperienceBusyTx(ctx, tx, d.Target.Name, ""); err != nil {
		return result, d, "", err
	}
	now := s.discovery.now().UTC()
	p, err := s.experienceSource.PromotionTx(ctx, tx, d.Evidence[0].Experience.MatchID, d.Options.RepeatThreshold, now.Add(-time.Duration(d.Options.RepeatWindowDays)*24*time.Hour), now)
	if err != nil {
		return result, d, "", err
	}
	if p.Relation != "new_capability" || p.Manual || p.Name != d.Target.Name {
		return result, d, "", errDiscoveryEvidence
	}
	target, err := s.experienceSource.NewPromotionTargetTx(ctx, tx, p.SessionID, d.Target.SourceID, p.Name)
	if err != nil {
		return result, d, "", err
	}
	if target != d.Target {
		return result, d, "", harness.ErrPermissionDenied
	}
	for _, ref := range original.Evidence {
		found := false
		for _, live := range p.Evidence {
			found = found || live.RootID == ref.RootID && encodedHash(live.Experience) == encodedHash(ref.Experience)
		}
		if !found {
			return result, d, "", errDiscoveryEvidence
		}
	}
	if err = verifyDiscoveryEpisodesTx(ctx, tx, original); err != nil {
		return result, d, "", err
	}
	result = harness.SkillLearningNewSkillRetryPreparation{
		Request: harness.SkillLearningNewSkillRetryRequest{PatternID: request.PatternID, ExpectedPatternRevision: request.ExpectedPatternRevision,
			ExpectedStateVersion: request.ExpectedStateVersion, Scope: request.Scope, PreviousGenerationID: previous.job.ID,
			ExpectedGenerationStateVersion: previous.job.StateVersion, PreviousProposalID: previous.proposal, ExpectedProposalStateVersion: proposalVersion,
			Target: d.Target, ExpectedHead: head, OverlayBase: overlay, EvidenceReservationID: original.ID, ExpectedRetryNumber: ordinal},
		PreviousGenerationState: previous.job.State, PreviousProposalStatus: status,
		Retry: harness.SkillLearningNewSkillRetryReference{PreviousGenerationID: previous.job.ID, EvidenceReservationID: original.ID, RetryNumber: ordinal}, MaxRetries: learning.MaxNewSkillRetries}
	return result, d, automatic, nil
}

func (s *Service) RetryNewSkill(ctx context.Context, actor string, request harness.SkillLearningNewSkillRetryRequest) (harness.SkillLearningNewSkillRetryResult, error) {
	var result harness.SkillLearningNewSkillRetryResult
	if s.experienceSource == nil {
		return result, harness.ErrPermissionDenied
	}
	// Receipt lookup precedes live paid-work gates. It cannot enqueue anything.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	saved, err := s.experienceSource.NewSkillRetryReceiptTx(ctx, tx, actor, request)
	_ = tx.Rollback()
	if err != nil || saved != nil {
		if saved != nil {
			result = *saved
		}
		return result, err
	}
	err = s.withGeneration(ctx, request.PreviousGenerationID, func(pub *skills.PublicationTx, previous generationRecord) error {
		tx := pub.SQL()
		if saved, err := s.experienceSource.NewSkillRetryReceiptTx(ctx, tx, actor, request); err != nil || saved != nil {
			if saved != nil {
				result = *saved
			}
			return err
		}
		prepared, prior, automatic, err := s.prepareCreationRetryTx(ctx, pub, actor, retryScopeRequest(request), previous)
		if err != nil {
			return err
		}
		prepared.Request.RequestKey = request.RequestKey
		if encodedHash(prepared.Request) != encodedHash(request) {
			return harness.ErrSkillConflict
		}
		now := s.discovery.now().UTC()
		eligible, err := s.discoveryCapacityTx(ctx, tx, request.Target, now)
		if err != nil {
			return err
		}
		if !eligible {
			return harness.ErrSkillConflict
		}
		d := prior
		d.ID, d.CreatedAt, d.CreationRetry = "sd_"+rand.Text(), now, &prepared.Retry
		payload, err := json.Marshal(d)
		if err != nil || len(payload) > 32<<10 {
			return invalid("creation retry discovery exceeds bounds")
		}
		generationRequest := previous.input.Request
		generationRequest.RequestKey, generationRequest.Author = "creation-retry:"+d.ID, "host_creation_retry"
		review, err := s.experienceSource.NewSkillRetryScopeTx(ctx, tx, actor, retryScopeRequest(request))
		if err != nil {
			return err
		}
		review.StateVersion++
		binding := learning.NewSkillRetryBinding{DiscoveryID: d.ID, DiscoveryDigest: hash(payload), AutomaticGenerationID: automatic, Reference: prepared.Retry, CreatedAt: now}
		binding.AuthorizationDigest = learning.NewSkillRetryAuthorization(actor, request, review, binding)
		r := generationRecord{id: "sgi_" + rand.Text(), created: now, input: generationSnapshot{Request: generationRequest, Target: d.Target,
			GeneratorIdentity: s.generation.identity, ValidatorIdentity: s.generation.reviewIdentity, DiscoveryID: d.ID, DiscoveryDigest: hash(payload), BehaviorIdentity: s.behavior.bindingIdentity(d.Target), CreationRetryAuthorization: binding.AuthorizationDigest}}
		input, err := json.Marshal(r.input)
		if err != nil {
			return err
		}
		r.job, err = s.generation.store.EnqueueTx(ctx, tx, skillmaintenance.Request{SourceIdentity: d.Target.SourceIdentity, Name: d.Target.Name, Key: generationRequest.RequestKey, Reference: r.id, InputDigest: hash(input)})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_generation_inputs(id,job_id,source_key,name,request_key,request_sha,payload,progress_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.id, r.job.ID, d.Target.SourceIdentity, d.Target.Name, generationRequest.RequestKey, generationRequestHash(generationRequest, d.Target), input, generationProgress(r), now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_discoveries(id,source_key,name,job_id,payload,payload_sha,created_ns) VALUES(?,?,?,?,?,?,?)`, d.ID, d.Target.SourceIdentity, d.Target.Name, r.job.ID, payload, hash(payload), now.UnixNano()); err != nil {
			return err
		}
		if err = s.experienceSource.BindPromotionTx(ctx, tx, r.job.ID, d.Evidence[0].Experience.MatchID, request.PatternID); err != nil {
			return err
		}
		binding.GenerationID, binding.GenerationDigest = r.job.ID, hash(input)
		binding.GenerationSnapshot = input
		result, err = s.experienceSource.ReserveNewSkillRetryTx(ctx, tx, actor, request, binding)
		if err != nil {
			return err
		}
		return reserveDiscoveryCapacityTx(ctx, tx, d)
	})
	if err != nil {
		return harness.SkillLearningNewSkillRetryResult{}, err
	}
	return result, nil
}
