package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (s *Service) ConfigureExperienceDiscoverySource(source *learning.Store) error {
	if s.experienceSource != nil || s.generation == nil || s.discovery != nil || s.generation.lifecycle.done != nil || !source.PromotionEnabled() || !source.PromotionDatabase(s.db) {
		return invalid("experience evidence requires startup-only host learning authority")
	}
	s.experienceSource, s.generation.experienceSource = source, source
	return nil
}

func experienceSkippable(err error) bool {
	return discoverySkippable(err) || errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, sql.ErrNoRows)
}

func (s *Service) discoverExperiences(ctx context.Context) (bool, error) {
	if !s.discovery.options.ExperiencePatternsEnabled {
		return false, nil
	}
	var cursor string
	if err := s.db.QueryRowContext(ctx, `SELECT cursor FROM df_skill_discovery_experience_scan WHERE id=1`).Scan(&cursor); err != nil {
		return false, err
	}
	ids, err := s.experienceSource.PromotionIDsWithReviewDrafts(ctx, cursor, s.discovery.options.ExperienceNewSkillsEnabled, s.discovery.options.ExperienceReviewDraftsEnabled)
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		_, err = s.db.ExecContext(ctx, `UPDATE df_skill_discovery_experience_scan SET cursor='' WHERE id=1 AND cursor=?`, cursor)
		return false, err
	}
	var combined error
	for _, id := range ids {
		if ctx.Err() != nil {
			return true, errors.Join(combined, ctx.Err())
		}
		err = s.considerExperience(ctx, id)
		if !experienceSkippable(err) && !errors.Is(err, skillmaintenance.ErrBudget) {
			combined = errors.Join(combined, err)
		}
		result, err := s.db.ExecContext(ctx, `UPDATE df_skill_discovery_experience_scan SET cursor=? WHERE id=1 AND cursor=?`, id, cursor)
		if err != nil {
			return true, errors.Join(combined, err)
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return true, errors.Join(combined, err)
		}
		cursor = id
	}
	return true, combined
}

func (s *Service) considerExperience(ctx context.Context, id string) error {
	now := s.discovery.now().UTC()
	o := s.discovery.options
	from := now.Add(-time.Duration(o.RepeatWindowDays) * 24 * time.Hour)
	observation, err := s.experienceSource.PromotionHint(ctx, id)
	if err != nil {
		return err
	}
	sourceID, name := "", ""
	switch observation.Relation {
	case "body_gap", "trigger_mismatch":
		if observation.Target == nil || observation.Head == nil {
			return errDiscoveryEvidence
		}
		sourceID, name = observation.Target.SourceID, observation.Target.Name
	case "new_capability":
		if !o.ExperienceNewSkillsEnabled || !harness.ValidSkillCapabilityName(observation.SuggestedName) {
			return harness.ErrPermissionDenied
		}
		sourceID, name = o.NewSkillSourceID, observation.SuggestedName
	default:
		return harness.ErrPermissionDenied
	}
	target, ok := s.generation.allowed[sourceID]
	target.Name = name
	if !ok || observation.Target != nil && target != *observation.Target {
		return harness.ErrPermissionDenied
	}
	return s.registry.WithPublication(ctx, target.SourceID, target.Name, func(pub *skills.PublicationTx) error {
		if pub.Target() != target {
			return harness.ErrPermissionDenied
		}
		tx := pub.SQL()
		head, err := pub.Current()
		if err != nil {
			return err
		}
		overlay, err := pub.OverlayBase()
		if err != nil {
			return err
		}
		if observation.Head != nil && (head != *observation.Head || !skills.SameOverlayBase(overlay, observation.OverlayBase)) {
			return harness.ErrSkillConflict
		}
		var capacity bool
		// Keep cheap daily/cooldown exclusions before rebuilding source trees.
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT reservations FROM df_skill_discovery_days WHERE day=?),0)<?
 AND COALESCE((SELECT until_ns FROM df_skill_discovery_cooldowns WHERE source_key=? AND name=?),0)<=?`,
			now.Format("2006-01-02"), o.MaxDailyProposals, target.SourceIdentity, target.Name, now.UnixNano()).Scan(&capacity); err != nil || !capacity {
			return err
		}
		// A hint may only exclude work. Avoid rebuilding a whole historical
		// tree for a pattern whose lifetime creation slot is already consumed.
		if observation.Relation == "new_capability" {
			if err = s.experienceSource.GuardNewPromotionTx(ctx, tx, observation.PatternID, ""); err != nil {
				return err
			}
		}
		p, err := experiencePromotionTx(ctx, tx, s.experienceSource, id, o, from, now)
		if err != nil {
			return err
		}
		if p.Relation != observation.Relation || p.Manual != (observation.Origin == "host_adjudicated") {
			return errDiscoveryEvidence
		}
		if p.Relation == "new_capability" {
			if err = s.experienceSource.GuardNewPromotionTx(ctx, tx, p.Evidence[0].Experience.PatternID, ""); err != nil {
				return err
			}
			chosen, err := s.experienceSource.NewPromotionTargetTx(ctx, tx, p.SessionID, o.NewSkillSourceID, p.Name)
			if err != nil {
				return err
			}
			if chosen != target || head != (harness.SkillHead{}) || overlay != nil {
				return harness.ErrSkillConflict
			}
			if err = guardNewExperienceBusyTx(ctx, tx, target.Name, ""); err != nil {
				return err
			}
		} else if p.Target != target || head != p.Head || !skills.SameOverlayBase(overlay, p.OverlayBase) {
			return harness.ErrSkillConflict
		}
		refs := make([]harness.SkillDiscoveryEvidence, 0, o.RepeatThreshold)
		for _, ref := range p.Evidence {
			key, err := discoveryEpisodeTx(ctx, tx, ref.RootID)
			if err != nil {
				return err
			}
			if key != ref.Experience.IndependenceKey {
				return errDiscoveryEvidence
			}
			consumed, err := discoveryEpisodeConsumedTx(ctx, tx, ref.RootID, key, target, head.Version)
			if err != nil {
				return err
			}
			if consumed {
				continue
			}
			ref.EpisodeKey = key
			refs = append(refs, ref)
			if len(refs) == o.RepeatThreshold {
				break
			}
		}
		if len(refs) == 0 || len(refs) < o.RepeatThreshold && !o.ExperienceReviewDraftsEnabled {
			return nil
		}
		// Replacement and admission share one transaction. If capacity is still
		// unavailable, leave the old draft exactly as it was, including its CAS.
		if _, err = tx.ExecContext(ctx, `SAVEPOINT draft_successor`); err != nil {
			return err
		}
		previous, err := s.supersedeReviewDraftTx(ctx, tx, target, head, p, refs, now)
		if err != nil {
			return err
		}
		eligible, err := s.discoveryCapacityTx(ctx, tx, target, now)
		if err != nil {
			return err
		}
		if !eligible {
			_, err = tx.ExecContext(ctx, `ROLLBACK TO draft_successor; RELEASE draft_successor`)
			return err
		}
		d := discoverySnapshot{Kind: "experience_pattern", ID: "sd_" + rand.Text(), Target: target, Base: head, OverlayBase: overlay, Options: o, CreatedAt: now, Evidence: refs, EpisodeDedup: true, HostAdjudicated: p.Manual, ReviewDraft: len(refs) < o.RepeatThreshold}
		payload, err := json.Marshal(d)
		if err != nil || len(payload) > 32<<10 {
			return invalid("experience discovery exceeds bounds")
		}
		reason := "Propose an instruction-only SKILL.md correction from the independently observed procedure. Preserve all support files, permissions and unrelated behavior. Historical execution is unassessed, not operator-confirmed quality. Publication requires independent host approval and evaluation."
		if p.Relation == "new_capability" {
			reason = "Propose a new instruction-only skill from the independently observed reusable procedure with the host-pinned readable name. Supply only SKILL.md with a concrete applicability description. Do not add scripts, support files, permissions or task-specific data. Historical execution is unassessed, not operator-confirmed quality. This candidate requires human approval."
		}
		if d.ReviewDraft {
			reason = "Automatically draft only SKILL.md from the source-verified procedure. Evidence is below the independent-episode threshold and does not prove generalization, efficacy or quality. Preserve support files, permissions and unrelated behavior; exclude task-specific data. This draft always requires human approval, even when automatic publication is enabled."
		}
		request := harness.SkillGenerationRequest{SourceID: target.SourceID, Name: target.Name, RequestKey: "discovery:" + d.ID, Reason: reason, Author: "discovery", Base: head}
		r := generationRecord{id: "sgi_" + rand.Text(), created: now, input: generationSnapshot{Request: request, Target: target, OverlayBase: overlay, GeneratorIdentity: s.generation.identity, ValidatorIdentity: s.generation.reviewIdentity, DiscoveryID: d.ID, DiscoveryDigest: hash(payload), BehaviorIdentity: s.behavior.bindingIdentity(target)}}
		input, err := json.Marshal(r.input)
		if err != nil {
			return err
		}
		r.job, err = s.generation.store.EnqueueTx(ctx, tx, skillmaintenance.Request{SourceIdentity: target.SourceIdentity, Name: target.Name, Key: request.RequestKey, Reference: r.id, InputDigest: hash(input)})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_generation_inputs(id,job_id,source_key,name,request_key,request_sha,payload,progress_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.id, r.job.ID, target.SourceIdentity, target.Name, request.RequestKey, generationRequestHash(request, target), input, generationProgress(r), now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_discoveries(id,source_key,name,job_id,payload,payload_sha,created_ns) VALUES(?,?,?,?,?,?,?)`, d.ID, target.SourceIdentity, target.Name, r.job.ID, payload, hash(payload), now.UnixNano()); err != nil {
			return err
		}
		if previous != "" {
			if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_discovery_successors(previous_proposal_id,discovery_id) VALUES(?,?)`, previous, d.ID); err != nil {
				return err
			}
		}
		if err = s.experienceSource.BindPromotionTx(ctx, tx, r.job.ID, id, refs[0].Experience.PatternID); err != nil {
			return err
		}
		if p.Relation == "new_capability" {
			if err = s.experienceSource.ReserveNewPromotionTx(ctx, tx, refs[0].Experience.PatternID, r.job.ID); err != nil {
				return err
			}
		}
		if err = reserveDiscoveryEpisodesTx(ctx, tx, d); err != nil {
			return err
		}
		return reserveDiscoveryCapacityTx(ctx, tx, d)
	})
}

func (s *Service) supersedeReviewDraftTx(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, head harness.SkillHead, promotion learning.Promotion, refs []harness.SkillDiscoveryEvidence, now time.Time) (string, error) {
	if promotion.Manual || promotion.Relation == "new_capability" || len(refs) < s.discovery.options.RepeatThreshold {
		return "", nil
	}
	var id, discoveryID, jobID string
	err := tx.QueryRowContext(ctx, `SELECT p.id,d.id,g.job_id FROM df_skill_proposals p
 JOIN df_skill_generation_inputs g ON g.proposal_id=p.id JOIN df_skill_discoveries d ON d.job_id=g.job_id
 WHERE p.source_key=? AND p.name=? AND p.status='pending_review'
 AND json_extract(CAST(d.payload AS TEXT),'$.review_draft')=1 ORDER BY p.id LIMIT 1`, target.SourceIdentity, target.Name).Scan(&id, &discoveryID, &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	d, boundJob, sha, err := readDiscovery(ctx, tx, discoveryID)
	if err != nil {
		return "", err
	}
	r, err := readGeneration(ctx, tx, jobID)
	if err != nil {
		return "", err
	}
	old, err := readProposal(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if d.kind() != "experience_pattern" || !d.ReviewDraft || d.HostAdjudicated || !d.Base.Exists || d.Base != head || d.Target != target || len(d.Evidence) == 0 || d.Evidence[0].Experience == nil || refs[0].Experience == nil ||
		d.Evidence[0].Experience.PatternID != refs[0].Experience.PatternID || d.Evidence[0].Experience.PatternRevision >= refs[0].Experience.PatternRevision ||
		boundJob != jobID || r.job.State != "completed" || r.input.DiscoveryDigest != sha || r.input.DiscoveryID != discoveryID || r.proposal != id || r.candidate != old.CandidateVersion || old.Base != head || old.Target != target {
		return "", nil
	}
	previous := old.StateVersion
	old.Status, old.StateVersion, old.UpdatedAt = "stale", old.StateVersion+1, now
	old.Error = "Superseded by a fresh-evidence successor; original evidence and charges remain consumed."
	if err = updateProposal(ctx, tx, old, previous); err != nil {
		return "", err
	}
	return id, nil
}

func experiencePromotionTx(ctx context.Context, tx *sql.Tx, source *learning.Store, id string, options harness.SkillDiscoveryOptions, from, until time.Time) (learning.Promotion, error) {
	p, err := source.PromotionTx(ctx, tx, id, options.RepeatThreshold, from, until)
	if options.ExperienceReviewDraftsEnabled && errors.Is(err, harness.ErrPermissionDenied) && !errors.Is(err, skillmaintenance.ErrSourceActive) {
		return source.ReviewDraftPromotionTx(ctx, tx, id, from, until)
	}
	return p, err
}

func (s *Service) discoveryCapacityTx(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE df_skill_discovery_scan SET last_ns=? WHERE id=1 AND last_ns<=?`, now.UnixNano(), now.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, skillmaintenance.ErrClock
	}
	var until int64
	err = tx.QueryRowContext(ctx, `SELECT until_ns FROM df_skill_discovery_cooldowns WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if until > now.UnixNano() {
		return false, nil
	}
	var daily int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT reservations FROM df_skill_discovery_days WHERE day=?),0)`, now.Format("2006-01-02")).Scan(&daily); err != nil {
		return false, err
	}
	if daily >= s.discovery.options.MaxDailyProposals {
		return false, nil
	}
	pending, err := discoveryPending(ctx, tx)
	if err != nil || pending >= s.discovery.options.MaxPendingProposals {
		return false, err
	}
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE source_key=? AND name=? AND state IN ('queued','running','retry'))
 OR EXISTS(SELECT 1 FROM df_skill_proposals WHERE source_key=? AND name=? AND status IN ('validating','pending_review','publishing'))`, target.SourceIdentity, target.Name, target.SourceIdentity, target.Name).Scan(&busy)
	return !busy, err
}

func reserveDiscoveryCapacityTx(ctx context.Context, tx *sql.Tx, d discoverySnapshot) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_skill_discovery_days(day,reservations) VALUES(?,1) ON CONFLICT(day) DO UPDATE SET reservations=reservations+1`, d.CreatedAt.Format("2006-01-02")); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO df_skill_discovery_cooldowns(source_key,name,until_ns) VALUES(?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET until_ns=excluded.until_ns`, d.Target.SourceIdentity, d.Target.Name, d.CreatedAt.Add(time.Duration(d.Options.CooldownHours)*time.Hour).UnixNano())
	return err
}

func liveExperienceDiscovery(ctx context.Context, tx *sql.Tx, source *learning.Store, d discoverySnapshot, job string) ([]harness.SkillGenerationEvidence, error) {
	if source == nil || len(d.Evidence) == 0 || d.Evidence[0].Experience == nil {
		return nil, errDiscoveryEvidence
	}
	x := d.Evidence[0].Experience
	if d.CreationRetry != nil {
		binding, err := source.NewSkillRetryBindingTx(ctx, tx, x.PatternID, job)
		if err != nil {
			return nil, errors.Join(errDiscoveryEvidence, err)
		}
		if binding.DiscoveryID != d.ID || binding.DiscoveryDigest != encodedHash(d) || binding.Reference != *d.CreationRetry || !binding.CreatedAt.Equal(d.CreatedAt) {
			return nil, errDiscoveryEvidence
		}
		if _, _, err = creationRetryOriginalTx(ctx, tx, d); err != nil {
			return nil, err
		}
	}
	if err := source.GuardPromotionTx(ctx, tx, job, x.MatchID, x.PatternID); err != nil {
		if experienceSkippable(err) {
			err = errDiscoveryEvidence
		}
		return nil, err
	}
	p, err := experiencePromotionTx(ctx, tx, source, x.MatchID, d.Options, d.CreatedAt.Add(-time.Duration(d.Options.RepeatWindowDays)*24*time.Hour), d.CreatedAt)
	var activity error
	if errors.Is(err, skillmaintenance.ErrSourceActive) {
		activity, err = err, nil
	}
	if experienceSkippable(err) {
		return nil, errDiscoveryEvidence
	}
	if err != nil {
		return nil, err
	}
	if p.Manual != d.HostAdjudicated {
		return nil, errDiscoveryEvidence
	}
	if p.Relation == "new_capability" {
		if err = source.GuardNewPromotionTx(ctx, tx, x.PatternID, job); err != nil {
			if experienceSkippable(err) {
				err = errDiscoveryEvidence
			}
			return nil, err
		}
		if !d.Options.ExperienceNewSkillsEnabled || d.Target.SourceID != d.Options.NewSkillSourceID || d.Target.Name != p.Name || d.Base != (harness.SkillHead{}) || d.OverlayBase != nil {
			return nil, errDiscoveryEvidence
		}
		target, err := source.NewPromotionTargetTx(ctx, tx, p.SessionID, d.Options.NewSkillSourceID, p.Name)
		if experienceSkippable(err) {
			return nil, errDiscoveryEvidence
		}
		if err != nil {
			return nil, err
		}
		if target != d.Target {
			return nil, errDiscoveryEvidence
		}
		if err = guardNewExperienceBusyTx(ctx, tx, target.Name, job); err != nil {
			return nil, err
		}
	} else if p.Target != d.Target || p.Head != d.Base || !skills.SameOverlayBase(p.OverlayBase, d.OverlayBase) {
		return nil, errDiscoveryEvidence
	}
	for _, ref := range d.Evidence {
		if !slices.ContainsFunc(p.Evidence, func(e harness.SkillDiscoveryEvidence) bool {
			return e.RootID == ref.RootID && encodedHash(e.Experience) == encodedHash(ref.Experience)
		}) {
			return nil, errDiscoveryEvidence
		}
	}
	if activity != nil {
		return nil, activity
	}
	// Fit the existing envelope with ordered UTF-8-safe fragments. Only their
	// concatenation is a complete JSON document; all text remains untrusted.
	var out []harness.SkillGenerationEvidence
	text := p.Summary
	for i := 0; text != ""; i++ {
		n := min(len(text), 3600)
		for n < len(text) && n > 0 && text[n]&0xc0 == 0x80 {
			n--
		}
		part := text[:n]
		text = text[n:]
		out = append(out, harness.SkillGenerationEvidence{Reference: x.MatchID + "/part-" + strconv.Itoa(i+1), Summary: part})
	}
	return out, nil
}

// Be conservative about unpublished names across configured services. Do not
// resolve a collision by choosing another label or silently changing source.
func guardNewExperienceBusyTx(ctx context.Context, tx *sql.Tx, name, ownJob string) error {
	var busy bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE owner_kind='skill' AND name=? AND id<>? AND state IN ('queued','running','retry'))
 OR EXISTS(SELECT 1 FROM df_skill_proposals p WHERE p.name=? AND p.status IN ('validating','pending_review','publishing')
 AND NOT EXISTS(SELECT 1 FROM df_skill_generation_inputs i WHERE i.job_id=? AND i.source_key=p.source_key AND i.name=p.name
 AND (i.proposal_id=p.id OR (i.proposal_id='' AND p.request_key='generation/'||i.job_id
 AND json_extract(CAST(p.payload AS TEXT),'$.candidate_sha256')=i.candidate_version))))`, name, ownJob, name, ownJob).Scan(&busy)
	if err == nil && busy {
		err = errDiscoveryEvidence
	}
	return err
}
