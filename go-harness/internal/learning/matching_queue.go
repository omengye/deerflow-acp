package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const MatchingPolicyVersion = "procedural-skill-match-v1"

type matchChoice struct {
	ID          string             `json:"id"`
	Skill       skills.MatchSkill  `json:"skill"`
	ActualReads []harness.SkillRef `json:"actual_reads"`
}

type matchSnapshot struct {
	ID                 string                            `json:"id"`
	Session            string                            `json:"session"`
	Owner              string                            `json:"owner"`
	Pattern            patternDependency                 `json:"pattern"`
	Catalog            int64                             `json:"catalog_revision"`
	InventorySHA       string                            `json:"inventory_sha256"`
	Choices            []matchChoice                     `json:"choices"`
	View               harness.SkillExperienceMatchInput `json:"view"`
	MaxDailyModelCalls int64                             `json:"max_daily_model_calls"`
	MaxDailyTokens     int64                             `json:"max_daily_tokens"`
}

func (s *Store) ConfigureMatching(matcher harness.SkillExperienceMatcher, access *skills.MatchAccess) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.done != nil || s.consolidator == nil || s.options.Matching == nil || s.matcher != nil || matcher == nil || access == nil || !validText(matcher.Identity(), 128, true) {
		return invalid("matching requires startup-only explicit opt-in and host skill access")
	}
	s.matcher, s.matchAccess = matcher, access
	return nil
}

func (s *Store) matchInputLimit() int {
	if s.options.Matching != nil && s.options.Matching.MaxInputBytes != 0 {
		return s.options.Matching.MaxInputBytes
	}
	return 128 << 10
}

func (s *Store) QueueMatchOne(ctx context.Context) (bool, error) {
	if !s.enabled || s.matcher == nil || s.maintenance == nil {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ctx = withEvidenceCache(ctx, tx)
	var sequence, revision int64
	var id, session, owner string
	err = tx.QueryRowContext(ctx, `SELECT i.sequence,i.pattern_id,i.revision,p.session_id,p.owner_key
 FROM df_learning_match_inputs i JOIN df_learning_patterns p ON p.id=i.pattern_id
 JOIN df_learning_pattern_revisions r ON r.pattern_id=i.pattern_id AND r.revision=i.revision
 JOIN df_learning_consolidations c ON c.id=r.consolidation_id JOIN df_skill_maintenance_jobs j ON j.id=c.job_id
	WHERE i.state='pending' AND i.sequence>(SELECT cursor FROM df_learning_match_scan WHERE id=1) AND j.state='completed'
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_reviews review WHERE review.pattern_id=i.pattern_id
 AND (review.ignored=1 OR review.adjudication_revision=i.revision))
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_members o WHERE o.pattern_id=i.pattern_id AND o.revision=i.revision AND `+episodeTailActivePredicate+`)
 AND NOT EXISTS(SELECT 1 FROM df_learning_matches a JOIN df_skill_maintenance_jobs active ON active.id=a.job_id
 WHERE a.pattern_id=i.pattern_id AND active.state IN ('queued','retry','running'))
 ORDER BY i.sequence LIMIT 1`).Scan(&sequence, &id, &revision, &session, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return s.queueMatchReassessmentTx(ctx, tx)
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_match_scan SET cursor=? WHERE id=1`, sequence); err != nil {
		return false, err
	}
	p, err := s.patternTx(ctx, tx, id, revision, true)
	if err != nil {
		return matchQueueFailure(ctx, tx, sequence, err, "evidence_changed")
	}
	if p.Pattern.EvidenceState == "conflicting" || p.Pattern.EvidenceState == "ambiguous" || p.Pattern.EvidenceState == "unresolved" {
		return skipMatch(ctx, tx, sequence, "evidence_blocked")
	}
	inventory, choices, view, err := s.matchViewTx(ctx, tx, p)
	if err != nil {
		return matchQueueFailure(ctx, tx, sequence, err, "inventory_unavailable")
	}
	data, _ := json.Marshal(view)
	if len(data) > s.matchInputLimit() {
		return skipMatch(ctx, tx, sequence, "source_bounds")
	}
	return s.enqueueMatchTx(ctx, tx, sequence, session, owner, p, inventory, choices, view, nil)
}

func (s *Store) enqueueMatchTx(ctx context.Context, tx *sql.Tx, sequence int64, session, owner string, p patternSnapshot, inventory skills.MatchInventory, choices []matchChoice, view harness.SkillExperienceMatchInput, previous *matchSnapshot) (bool, error) {
	id, revision := p.Pattern.ID, p.Pattern.Revision
	snap := matchSnapshot{Session: session, Owner: owner, Pattern: patternDependency{ID: id, Revision: revision, SHA: encodedDigest(p)}, Catalog: inventory.Catalog, InventorySHA: encodedDigest(inventory), Choices: choices, View: view,
		MaxDailyModelCalls: s.options.MaxDailyModelCalls, MaxDailyTokens: s.options.MaxDailyTokens}
	snap.ID = "lmatch_" + encodedDigest([]any{MatchingPolicyVersion, s.matcher.Identity(), s.matchAccess.Identity(), snap})
	if previous != nil {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_matches WHERE id=?)`, snap.ID).Scan(&exists); err != nil || exists {
			if err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
	}
	payload, _ := json.Marshal(snap)
	job, err := s.maintenance.EnqueueTx(ctx, tx, skillmaintenance.Request{Kind: skillmaintenance.SkillMatch, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: owner, Key: snap.ID, Reference: snap.ID, InputDigest: digest(payload)})
	if errors.Is(err, skillmaintenance.ErrBudget) {
		_, err = tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET reason='queue_backpressure' WHERE sequence=?`, sequence)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_matches(id,session_id,pattern_id,revision,job_id,policy_version,model_identity,access_identity,payload,payload_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, snap.ID, session, id, revision, job.ID, MatchingPolicyVersion, s.matcher.Identity(), s.matchAccess.Identity(), payload, digest(payload), s.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if previous != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_match_reassessments(pattern_id,ordinal,previous_id,fresh_id)
 SELECT ?,COALESCE(MAX(ordinal),0)+1,?,? FROM df_learning_match_reassessments WHERE pattern_id=?`, id, previous.ID, snap.ID, id); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='queued',job_id=?,reason='' WHERE sequence=?`, job.ID, sequence)
	if err != nil {
		return false, err
	}
	if err = matchWriteApplied(result); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func matchQueueFailure(ctx context.Context, tx *sql.Tx, sequence int64, err error, reason string) (bool, error) {
	if errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return skipMatch(ctx, tx, sequence, reason)
	}
	return false, err
}

func skipMatch(ctx context.Context, tx *sql.Tx, sequence int64, reason string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='skipped',reason=? WHERE sequence=? AND state='pending'`, reason, sequence); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) matchViewTx(ctx context.Context, tx *sql.Tx, p patternSnapshot) (skills.MatchInventory, []matchChoice, harness.SkillExperienceMatchInput, error) {
	return s.matchTargetViewTx(ctx, tx, p, nil)
}

func (s *Store) matchTargetViewTx(ctx context.Context, tx *sql.Tx, p patternSnapshot, target *harness.SkillTarget) (skills.MatchInventory, []matchChoice, harness.SkillExperienceMatchInput, error) {
	ctx = withEvidenceCache(ctx, tx)
	view := harness.SkillExperienceMatchInput{PatternID: p.Pattern.ID, Revision: p.Pattern.Revision, Candidate: p.Pattern.Candidate, EvidenceState: p.Pattern.EvidenceState, Skills: []harness.SkillExperienceMatchChoice{}}
	var cwd string
	if err := tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, p.Pattern.SessionID).Scan(&cwd); err != nil {
		return skills.MatchInventory{}, nil, view, err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_key FROM df_learning_patterns WHERE id=? AND session_id=?`, p.Pattern.ID, p.Pattern.SessionID).Scan(&owner); err != nil {
		return skills.MatchInventory{}, nil, view, err
	}
	if workspaceKey(cwd) != owner {
		return skills.MatchInventory{}, nil, view, harness.ErrPermissionDenied
	}
	inventory, err := s.matchAccess.InventoryTx(ctx, tx, cwd)
	if err != nil {
		return inventory, nil, view, err
	}
	if target != nil && !slices.ContainsFunc(inventory.Skills, func(e skills.MatchSkill) bool { return e.Target == *target }) {
		return inventory, nil, view, harness.ErrPermissionDenied
	}
	reads := map[harness.SkillRef]bool{}
	for _, dep := range p.Members {
		var jobID string
		if err = tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_extractions WHERE id=?`, dep.Extraction).Scan(&jobID); err != nil {
			return inventory, nil, view, err
		}
		job, readErr := skillmaintenance.ReadJob(ctx, tx, jobID)
		if readErr != nil {
			return inventory, nil, view, readErr
		}
		extraction, _, readErr := s.readExtractionTx(ctx, tx, job)
		if readErr != nil {
			return inventory, nil, view, readErr
		}
		for _, ref := range extraction.View.References {
			if ref.Kind == "skill_read" && ref.Skill != nil {
				reads[*ref.Skill] = true
			}
		}
		if len(reads) > 32 {
			return inventory, nil, view, invalid("matching read inventory exceeds bounds")
		}
	}
	type ranked struct {
		skill skills.MatchSkill
		reads []harness.SkillRef
		score int
	}
	query := proceduralTokens(p.Pattern.Candidate)
	ranking := make([]ranked, 0, len(inventory.Skills))
	for _, e := range inventory.Skills {
		r := ranked{skill: e, reads: []harness.SkillRef{}}
		for ref := range reads {
			if ref.SourceID == e.Target.SourceID && ref.SourceIdentity == e.Target.SourceIdentity && ref.Name == e.Target.Name {
				r.reads = append(r.reads, ref)
			}
		}
		sort.Slice(r.reads, func(i, j int) bool { return r.reads[i].Version < r.reads[j].Version })
		for token := range proceduralTokens(harness.SkillExperienceCandidate{Goal: e.Target.Name, Applicability: e.Description}) {
			if query[token] {
				r.score++
			}
		}
		ranking = append(ranking, r)
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		if target != nil && (ranking[i].skill.Target == *target) != (ranking[j].skill.Target == *target) {
			return ranking[i].skill.Target == *target
		}
		if (len(ranking[i].reads) != 0) != (len(ranking[j].reads) != 0) {
			return len(ranking[i].reads) != 0
		}
		return ranking[i].score > ranking[j].score
	})
	view.InventoryComplete = len(ranking) <= 8
	choices := make([]matchChoice, 0, min(len(ranking), 8))
	for _, r := range ranking[:min(len(ranking), 8)] {
		body, readErr := s.matchAccess.BodyTx(ctx, tx, inventory, r.skill)
		if readErr != nil {
			return inventory, nil, view, readErr
		}
		id := "choice_" + encodedDigest(r.skill)
		fixed := r.skill
		fixed.Description = redact(fixed.Description, 4096)
		choices = append(choices, matchChoice{ID: id, Skill: fixed, ActualReads: r.reads})
		view.Skills = append(view.Skills, harness.SkillExperienceMatchChoice{ID: id, Name: r.skill.Target.Name, Description: fixed.Description, Body: redact(body, skills.MaxMatchBodyBytes), ActuallyRead: len(r.reads) != 0,
			CurrentVersionRead: slices.ContainsFunc(r.reads, func(ref harness.SkillRef) bool { return ref.Version == r.skill.Head.Version })})
	}
	return inventory, choices, view, nil
}
