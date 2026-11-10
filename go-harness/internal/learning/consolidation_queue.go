package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type patternDependency struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	SHA      string `json:"sha256"`
}

var errPatternScanProgress = errors.New("pattern metadata scan has more pages")

var errConsolidationDeferred = errors.New("learning consolidation source is active")

type consolidationSnapshot struct {
	ID               string                                    `json:"id"`
	Session          string                                    `json:"session"`
	Owner            string                                    `json:"owner"`
	Shared           bool                                      `json:"workspace_shared,omitempty"`
	ResolutionPolicy string                                    `json:"resolution_policy,omitempty"`
	Seed             experienceDependency                      `json:"seed"`
	Patterns         []patternDependency                       `json:"patterns"`
	View             harness.SkillExperienceConsolidationInput `json:"view"`
}

func (s *Store) ConfigureConsolidation(consolidator harness.SkillExperienceConsolidator) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.done != nil || s.extractor == nil || s.options.Consolidation == nil || s.consolidator != nil || consolidator == nil || !validText(consolidator.Identity(), 128, true) {
		return invalid("consolidation requires startup-only explicit opt-in")
	}
	s.consolidator = consolidator
	return nil
}

// Advance already-paid output first; each stage gets one bounded scan inside
// the shared learning slots, without opening another worker pool.
func (s *Store) QueueLearning(ctx context.Context) (bool, error) {
	a, first := s.QueueMatchOne(ctx)
	if ctx.Err() != nil {
		return a, first
	}
	b, second := s.QueueConsolidationOne(ctx)
	if ctx.Err() != nil {
		return a || b, errors.Join(first, second)
	}
	c, third := s.QueueOne(ctx)
	return a || b || c, errors.Join(first, second, third)
}

func (s *Store) QueueConsolidationOne(ctx context.Context) (bool, error) {
	if !s.enabled || s.consolidator == nil || s.maintenance == nil {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ctx = withEvidenceCache(ctx, tx)
	var id, session, owner string
	err = tx.QueryRowContext(ctx, `SELECT i.experience_id,e.session_id,p.workspace_key FROM df_learning_consolidation_inputs i
 JOIN df_learning_experiences e ON e.id=i.experience_id JOIN df_learning_extractions o ON o.id=e.extraction_id
 JOIN df_learning_episodes p ON p.id=e.episode_id JOIN df_skill_maintenance_jobs extracted ON extracted.id=o.job_id
 JOIN harness_learning_controls c ON c.session_id=e.session_id
 WHERE i.state='pending' AND i.experience_id>(SELECT cursor FROM df_learning_consolidation_scan WHERE id=1)
 AND c.contribute=1 AND c.epoch=o.epoch AND extracted.state='completed'
 AND NOT (`+episodeTailActivePredicate+`)
 AND NOT EXISTS(SELECT 1 FROM df_learning_consolidations a JOIN df_skill_maintenance_jobs j ON j.id=a.job_id
 WHERE j.state IN ('queued','retry','running') AND (a.session_id=e.session_id OR (? AND
 EXISTS(SELECT 1 FROM harness_learning_sharing sh WHERE sh.session_id=e.session_id AND sh.workspace_shared=1) AND
 EXISTS(SELECT 1 FROM df_learning_consolidation_sharing sh WHERE sh.job_id=a.job_id AND sh.workspace_key=p.workspace_key))))
 ORDER BY i.experience_id LIMIT 1`, s.options.CrossSession).Scan(&id, &session, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_scan SET cursor='' WHERE id=1`)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_scan SET cursor=? WHERE id=1`, id); err != nil {
		return false, err
	}
	e, dep, err := s.experienceTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, sql.ErrNoRows) {
			return skipConsolidation(ctx, tx, id, "evidence_changed")
		}
		return false, err
	}
	shared, err := s.seedSharedTx(ctx, tx, dep, owner)
	if err != nil {
		return skipConsolidation(ctx, tx, id, "sharing_changed")
	}
	choices, err := s.consolidationShortlistTx(ctx, tx, session, owner, shared, e.Candidate)
	if err != nil {
		if errors.Is(err, errPatternScanProgress) {
			if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET reason='search_incomplete' WHERE experience_id=?`, id); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
		if errors.Is(err, errConsolidationDeferred) {
			if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET reason='tree_unsettled' WHERE experience_id=? AND state='pending'`, id); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
		if errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, sql.ErrNoRows) {
			return skipConsolidation(ctx, tx, id, "shortlist_evidence_changed")
		}
		return false, err
	}
	if choices == nil {
		return skipConsolidation(ctx, tx, id, "pattern_capacity")
	}
	snap := consolidationSnapshot{Session: session, Owner: owner, Shared: shared, ResolutionPolicy: ResolutionPolicyVersion, Seed: dep, Patterns: []patternDependency{}, View: harness.SkillExperienceConsolidationInput{Seed: harness.SkillExperienceConsolidationSeed{ID: e.ID, EpisodeID: e.EpisodeID, Candidate: e.Candidate}, Patterns: []harness.SkillExperienceConsolidationChoice{}}}
	for _, p := range choices {
		snap.Patterns = append(snap.Patterns, patternDependency{ID: p.Pattern.ID, Revision: p.Pattern.Revision, SHA: encodedDigest(p)})
		snap.View.Patterns = append(snap.View.Patterns, harness.SkillExperienceConsolidationChoice{ID: p.Pattern.ID, Revision: p.Pattern.Revision, Candidate: p.Pattern.Candidate, EvidenceState: p.Pattern.EvidenceState})
	}
	view, _ := json.Marshal(snap.View)
	if len(view) > s.consolidationInputLimit() {
		return skipConsolidation(ctx, tx, id, "source_bounds")
	}
	snap.ID = "lcon_" + encodedDigest([]any{ConsolidationPolicyVersion, s.consolidator.Identity(), snap})
	payload, err := json.Marshal(snap)
	if err != nil {
		return false, err
	}
	job, err := s.maintenance.EnqueueTx(ctx, tx, skillmaintenance.Request{Kind: skillmaintenance.ExperienceConsolidate, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: owner, Key: snap.ID, Reference: snap.ID, InputDigest: digest(payload)})
	if errors.Is(err, skillmaintenance.ErrCapacity) {
		_, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET reason='queue_backpressure' WHERE experience_id=?`, id)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_consolidations(id,session_id,experience_id,job_id,policy_version,model_identity,payload,payload_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, snap.ID, session, id, job.ID, ConsolidationPolicyVersion, s.consolidator.Identity(), payload, digest(payload), s.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	if err = saveConsolidationSourcesTx(ctx, tx, job.ID, snap, choices); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='queued',job_id=?,reason='' WHERE experience_id=? AND state='pending'`, job.ID, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func skipConsolidation(ctx context.Context, tx *sql.Tx, id, reason string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='skipped',reason=? WHERE experience_id=? AND state='pending'`, reason, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) consolidationInputLimit() int {
	if s.options.Consolidation != nil && s.options.Consolidation.MaxInputBytes != 0 {
		return s.options.Consolidation.MaxInputBytes
	}
	return 128 << 10
}

func (s *Store) consolidationShortlistTx(ctx context.Context, tx *sql.Tx, session, owner string, shared bool, seed harness.SkillExperienceCandidate) ([]patternSnapshot, error) {
	type ranked struct {
		ID       string
		Revision int64
		Score    int
	}
	key := encodedDigest([]any{session, owner, shared, seed})
	var catalog int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM df_learning_pattern_catalog WHERE id=1`).Scan(&catalog); err != nil {
		return nil, err
	}
	var prior int64
	var cursor, sha string
	var saved []byte
	valid := make([]ranked, 0, 8)
	err := tx.QueryRowContext(ctx, `SELECT catalog,cursor,ranked,ranked_sha FROM df_learning_pattern_scan_progress WHERE key=?`, key).Scan(&prior, &cursor, &saved, &sha)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && prior == catalog {
		if digest(saved) != sha || json.Unmarshal(saved, &valid) != nil || len(valid) > 8 {
			return nil, invalid("pattern scan checkpoint integrity failure")
		}
	} else {
		cursor = ""
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.current_revision,r.payload,r.payload_sha FROM df_learning_patterns p
	JOIN df_learning_pattern_revisions r ON r.pattern_id=p.id AND r.revision=p.current_revision
 LEFT JOIN df_learning_pattern_sharing sh ON sh.pattern_id=p.id
 WHERE (? AND sh.workspace_key=? AND p.owner_key=? OR NOT ? AND p.session_id=? AND sh.pattern_id IS NULL) AND p.id>?
 ORDER BY p.id LIMIT 128`, shared, owner, owner, shared, session, cursor)
	if err != nil {
		return nil, err
	}
	count := 0
	query := proceduralTokens(seed)
	for rows.Next() {
		var r ranked
		var data []byte
		var sha string
		if err = rows.Scan(&r.ID, &r.Revision, &data, &sha); err != nil {
			break
		}
		var p patternSnapshot
		if len(data) > patternStorageLimit || digest(data) != sha || json.Unmarshal(data, &p) != nil || p.Pattern.ID != r.ID || p.Pattern.Revision != r.Revision || p.Shared != shared || !shared && p.Pattern.SessionID != session {
			err = invalid("inconsistent consolidation inventory")
			break
		}
		for token := range proceduralTokens(p.Pattern.Candidate) {
			if query[token] {
				r.Score++
			}
		}
		// Keep a bounded working set while scanning the entire authorized index.
		count++
		cursor = r.ID
		valid = append(valid, r)
		sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
		if len(valid) > 8 {
			valid = valid[:8]
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if count == 128 {
		data, _ := json.Marshal(valid)
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_pattern_scan_progress VALUES(?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET catalog=excluded.catalog,cursor=excluded.cursor,ranked=excluded.ranked,ranked_sha=excluded.ranked_sha`, key, catalog, cursor, data, digest(data)); err != nil {
			return nil, err
		}
		return nil, errPatternScanProgress
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM df_learning_pattern_scan_progress WHERE key=?`, key); err != nil {
		return nil, err
	}
	// Recent candidates remain a fallback for synonyms with no lexical overlap.
	// Recall never proves equivalence; the bounded model must compare boundaries.
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
	result := make([]patternSnapshot, 0, min(len(valid), 8))
	for _, r := range valid[:min(len(valid), 8)] {
		var unfinished bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_pattern_members o
 WHERE o.pattern_id=? AND o.revision<=? AND `+episodeTailActivePredicate+`)`, r.ID, r.Revision).Scan(&unfinished); err != nil {
			return nil, err
		}
		if unfinished {
			return nil, errConsolidationDeferred
		}
		p, err := s.patternTx(ctx, tx, r.ID, r.Revision, true)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func proceduralTokens(c harness.SkillExperienceCandidate) map[string]bool {
	text := strings.ToLower(strings.Join([]string{c.Goal, c.Applicability, c.Problem, c.Resolution, c.Verification, strings.Join(c.Steps, " ")}, " "))
	tokens := map[string]bool{}
	var word []rune
	flush := func() {
		if len(word) >= 2 {
			tokens[string(word)] = true
		}
		word = word[:0]
	}
	var prior rune
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			flush()
			if prior != 0 {
				tokens[string([]rune{prior, r})] = true
			}
			prior = r
		} else {
			prior = 0
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				word = append(word, r)
			} else {
				flush()
			}
		}
	}
	flush()
	return tokens
}
