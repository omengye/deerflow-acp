package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const MatchingPolicyVersion = "procedural-skill-match-v3"

type matchChoice struct {
	ID          string             `json:"id"`
	Skill       skills.MatchSkill  `json:"skill"`
	ActualReads []harness.SkillRef `json:"actual_reads"`
}

type matchSnapshot struct {
	ID           string                            `json:"id"`
	Session      string                            `json:"session"`
	Owner        string                            `json:"owner"`
	Pattern      patternDependency                 `json:"pattern"`
	Catalog      int64                             `json:"catalog_revision"`
	InventorySHA string                            `json:"inventory_sha256"`
	Choices      []matchChoice                     `json:"choices"`
	View         harness.SkillExperienceMatchInput `json:"view"`
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
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_members o WHERE o.pattern_id=i.pattern_id AND o.revision<=i.revision AND `+episodeTailActivePredicate+`)
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
	snap := matchSnapshot{Session: session, Owner: owner, Pattern: patternDependency{ID: id, Revision: revision, SHA: encodedDigest(p)}, Catalog: inventory.Catalog, InventorySHA: encodedDigest(inventory), Choices: choices, View: view}
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
	if errors.Is(err, skillmaintenance.ErrCapacity) {
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
	if errors.Is(err, errMatchCatalogProgress) {
		_, e := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET reason='search_incomplete' WHERE sequence=?`, sequence)
		if e != nil {
			return false, e
		}
		return true, tx.Commit()
	}
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
	return s.matchPageTargetViewTx(ctx, tx, p, nil, -1)
}

func (s *Store) matchTargetViewTx(ctx context.Context, tx *sql.Tx, p patternSnapshot, target *harness.SkillTarget) (skills.MatchInventory, []matchChoice, harness.SkillExperienceMatchInput, error) {
	return s.matchPageTargetViewTx(ctx, tx, p, target, 0)
}

func (s *Store) matchPageTargetViewTx(ctx context.Context, tx *sql.Tx, p patternSnapshot, target *harness.SkillTarget, offset int) (skills.MatchInventory, []matchChoice, harness.SkillExperienceMatchInput, error) {
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
	inventory, scanKey, err := s.matchCatalogTx(ctx, tx, p, cwd)
	if err != nil {
		return inventory, nil, view, err
	}
	var inventorySize int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_learning_match_catalog_entries WHERE scan_key=?`, scanKey).Scan(&inventorySize); err != nil {
		return inventory, nil, view, err
	}
	if offset < 0 {
		var catalog int64
		var sha string
		err = tx.QueryRowContext(ctx, `SELECT catalog,inventory_sha,next_offset FROM df_learning_match_progress WHERE pattern_id=? AND revision=?`, p.Pattern.ID, p.Pattern.Revision).Scan(&catalog, &sha, &offset)
		if errors.Is(err, sql.ErrNoRows) {
			offset = 0
			err = nil
		}
		if err != nil {
			return inventory, nil, view, err
		}
		if catalog != inventory.Catalog || sha != encodedDigest(inventory) {
			offset = 0
		}
	}
	if offset < 0 || offset > inventorySize {
		return inventory, nil, view, invalid("invalid search cursor")
	}
	if err = verifiedMatchPrefixTx(ctx, tx, p.Pattern.ID, p.Pattern.Revision, inventory.Catalog, encodedDigest(inventory), offset); err != nil {
		return inventory, nil, view, err
	}
	view.PageOffset, view.InventorySize = offset, inventorySize
	targetName := ""
	if target != nil {
		targetName = target.Name
		grant, e := s.matchAccess.InventoryNameTx(ctx, tx, cwd, target.Name)
		if e != nil {
			return inventory, nil, view, e
		}
		if !slices.ContainsFunc(grant.Skills, func(e skills.MatchSkill) bool { return e.Target == *target }) {
			return inventory, nil, view, harness.ErrPermissionDenied
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,payload_sha FROM df_learning_match_catalog_entries WHERE scan_key=? ORDER BY (name=?) DESC,was_read DESC,score DESC,name LIMIT 8 OFFSET ?`, scanKey, targetName, offset)
	if err != nil {
		return inventory, nil, view, err
	}
	var ranking []matchChoice
	for rows.Next() {
		var raw []byte
		var sha string
		var choice matchChoice
		if err = rows.Scan(&raw, &sha); err != nil {
			break
		}
		if len(raw) > 64<<10 || digest(raw) != sha || json.Unmarshal(raw, &choice) != nil {
			err = invalid("catalog page integrity failure")
			break
		}
		ranking = append(ranking, choice)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return inventory, nil, view, err
	}
	view.InventoryComplete = offset+len(ranking) >= inventorySize
	choices := make([]matchChoice, 0, min(len(ranking), 8))
	for _, r := range ranking {
		grant, readErr := s.matchAccess.InventoryNameTx(ctx, tx, cwd, r.Skill.Target.Name)
		if readErr != nil {
			return inventory, nil, view, readErr
		}
		body, readErr := s.matchAccess.BodyTx(ctx, tx, grant, r.Skill)
		if readErr != nil {
			return inventory, nil, view, readErr
		}
		id := "choice_" + encodedDigest(r.Skill)
		fixed := r.Skill
		fixed.Description = redact(fixed.Description, 4096)
		choices = append(choices, matchChoice{ID: id, Skill: fixed, ActualReads: r.ActualReads})
		view.Skills = append(view.Skills, harness.SkillExperienceMatchChoice{ID: id, Name: r.Skill.Target.Name, Description: fixed.Description, Body: redact(body, skills.MaxMatchBodyBytes), ActuallyRead: len(r.ActualReads) != 0,
			CurrentVersionRead: slices.ContainsFunc(r.ActualReads, func(ref harness.SkillRef) bool { return ref.Version == r.Skill.Head.Version })})
		if data, _ := json.Marshal(view); len(data) > s.matchInputLimit() {
			view.Skills = view.Skills[:len(view.Skills)-1]
			choices = choices[:len(choices)-1]
			if len(choices) == 0 {
				return inventory, nil, view, invalid("matching body exceeds context capacity")
			}
			view.InventoryComplete = false
			break
		}
	}
	return inventory, choices, view, nil
}
