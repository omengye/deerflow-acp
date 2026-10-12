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
)

type matchResult struct {
	Decision harness.SkillExperienceMatchDecision `json:"decision"`
	Match    harness.SkillExperienceMatch         `json:"match"`
}

func validateMatchDecision(input harness.SkillExperienceMatchInput, d harness.SkillExperienceMatchDecision) (harness.SkillExperienceMatchDecision, error) {
	if !validText(d.Reason, 2000, true) {
		return d, invalid("invalid matching explanation")
	}
	switch d.Relation {
	case "body_gap", "trigger_mismatch", "already_covered":
		if !slices.ContainsFunc(input.Skills, func(e harness.SkillExperienceMatchChoice) bool { return e.ID == d.SkillID }) {
			return d, invalid("invented matching target")
		}
	case "new_capability", "irrelevant", "ambiguous":
		if d.SkillID != "" || d.Relation == "new_capability" && !input.InventoryComplete {
			return d, invalid("untargeted match or incomplete capability search")
		}
	default:
		return d, invalid("unsupported matching relation")
	}
	if d.SuggestedName != "" && (d.Relation != "new_capability" || !harness.ValidSkillCapabilityName(d.SuggestedName) || redact(d.SuggestedName, 64) != d.SuggestedName) {
		return d, invalid("invalid capability name suggestion")
	}
	d.Reason = redact(d.Reason, 2000)
	if !validText(d.Reason, 2000, true) {
		return d, invalid("empty redacted matching explanation")
	}
	return d, nil
}

func planMatch(snap matchSnapshot, d harness.SkillExperienceMatchDecision, now time.Time) (harness.SkillExperienceMatch, error) {
	m := harness.SkillExperienceMatch{ID: snap.ID, SessionID: snap.Session, Origin: "model_matched", Quality: "unassessed", PatternID: snap.Pattern.ID, PatternRevision: snap.Pattern.Revision, Relation: d.Relation, Reason: d.Reason, SuggestedName: d.SuggestedName, ActualReads: []harness.SkillRef{}, EvidenceState: snap.View.EvidenceState, SourceState: "checked_at_matching", CreatedAt: now}
	if d.SkillID == "" {
		return m, nil
	}
	for _, c := range snap.Choices {
		if c.ID == d.SkillID {
			target, head := c.Skill.Target, c.Skill.Head
			m.Target, m.Head = &target, &head
			if c.Skill.OverlayBase != nil {
				base := *c.Skill.OverlayBase
				m.OverlayBase = &base
			}
			m.ActualReads = slices.Clone(c.ActualReads)
			return m, nil
		}
	}
	return m, invalid("missing fixed matching target")
}

// Verify storage and the current input binding before classifying any live
// inventory change. A corrupt result is never grounds for another paid job.
func (s *Store) readMatchRecordTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job, reassess bool) (matchSnapshot, *matchResult, error) {
	var snap matchSnapshot
	var data, output []byte
	var sha, resultSHA, policy, model, access, session, pattern string
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result,result_sha,policy_version,model_identity,access_identity,session_id,pattern_id,revision
 FROM df_learning_matches WHERE id=? AND job_id=?`, job.Request.Reference, job.ID).Scan(&data, &sha, &output, &resultSHA, &policy, &model, &access, &session, &pattern, &revision)
	if err != nil {
		return snap, nil, err
	}
	if len(data) > s.matchInputLimit()+(64<<10) || digest(data) != sha || sha != job.Request.InputDigest || json.Unmarshal(data, &snap) != nil || snap.ID != job.Request.Reference || job.Request.Key != snap.ID || job.Request.Kind != skillmaintenance.SkillMatch || job.Request.OwnerKind != skillmaintenance.WorkspaceOwner || job.Request.OwnerKey != snap.Owner || snap.Session != session || snap.Pattern.ID != pattern || snap.Pattern.Revision != revision || snap.MaxDailyModelCalls < 1 || snap.MaxDailyTokens < 1 || len(snap.Choices) > 8 || len(snap.Choices) != len(snap.View.Skills) {
		return snap, nil, invalid("inconsistent matching identity")
	}
	identity := snap
	identity.ID = ""
	if snap.ID != "lmatch_"+encodedDigest([]any{policy, model, access, identity}) {
		return snap, nil, invalid("inconsistent fixed matching digest")
	}
	if !s.enabled || s.options.Matching == nil || s.matcher == nil || s.matchAccess == nil || policy != MatchingPolicyVersion || model != s.matcher.Identity() || access != s.matchAccess.Identity() {
		return snap, nil, harness.ErrSkillConflict
	}
	var state, jobID string
	if err = tx.QueryRowContext(ctx, `SELECT state,job_id FROM df_learning_match_inputs WHERE pattern_id=? AND revision=?`, pattern, revision).Scan(&state, &jobID); err != nil || jobID != job.ID {
		if err == nil {
			err = harness.ErrPermissionDenied
		}
		return snap, nil, err
	}
	if resultSHA == "" {
		invalidated := reassess && state == "skipped" && job.State == "failed" && job.ErrorCode == "inventory_changed"
		if len(output) != 0 || state != "queued" && !invalidated {
			return snap, nil, invalid("uncommitted matching result or missing acknowledgment")
		}
		return snap, nil, nil
	}
	var result matchResult
	if len(output) > 32<<10 || digest(output) != resultSHA || json.Unmarshal(output, &result) != nil || result.Match.CreatedAt.IsZero() || state != "processed" {
		return snap, nil, invalid("inconsistent matching result")
	}
	d, err := validateMatchDecision(snap.View, result.Decision)
	if err != nil || encodedDigest(d) != encodedDigest(result.Decision) {
		return snap, nil, invalid("inconsistent saved matching decision")
	}
	expected, err := planMatch(snap, d, result.Match.CreatedAt)
	if err != nil || encodedDigest(expected) != encodedDigest(result.Match) {
		return snap, nil, invalid("inconsistent saved matching projection")
	}
	return snap, &result, nil
}

func (s *Store) readMatchTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) (out matchSnapshot, saved *matchResult, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	snap, result, err := s.readMatchRecordTx(ctx, tx, job, false)
	if err != nil {
		return snap, nil, err
	}
	review, err := patternReviewTx(ctx, tx, snap.Pattern.ID)
	if err != nil {
		return snap, nil, err
	}
	if review.Ignored || review.AdjudicationRevision == snap.Pattern.Revision {
		return snap, nil, harness.ErrPermissionDenied
	}
	p, err := s.patternTx(ctx, tx, snap.Pattern.ID, snap.Pattern.Revision, result == nil)
	if err != nil {
		return snap, nil, err
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner_key FROM df_learning_patterns WHERE id=?`, snap.Pattern.ID).Scan(&owner); err != nil {
		return snap, nil, err
	}
	if owner != snap.Owner || p.Pattern.SessionID != snap.Session || encodedDigest(p) != snap.Pattern.SHA {
		return snap, nil, harness.ErrPermissionDenied
	}
	inventory, choices, view, err := s.matchViewTx(ctx, tx, p)
	if err != nil {
		if errors.Is(err, harness.ErrNotFound) {
			err = harness.ErrSkillConflict
		}
		return snap, nil, err
	}
	if inventory.Catalog != snap.Catalog || encodedDigest(inventory) != snap.InventorySHA || encodedDigest(choices) != encodedDigest(snap.Choices) || encodedDigest(view) != encodedDigest(snap.View) {
		return snap, nil, errors.Join(skillmaintenance.ErrMatchInventoryChanged, harness.ErrSkillConflict)
	}
	return snap, result, nil
}

func (s *Store) HandleMatch(ctx context.Context, job skillmaintenance.Job) (string, bool) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	snap, saved, err := s.readMatchTx(ctx, tx, job)
	if err == nil {
		err = skillmaintenance.GuardTx(ctx, tx, job.ID)
	}
	_ = tx.Rollback()
	if err != nil {
		return learningFailure(err)
	}
	if saved != nil {
		return "", false
	}
	data, _ := json.Marshal(snap.View)
	var view harness.SkillExperienceMatchInput
	if err = json.Unmarshal(data, &view); err != nil {
		return learningFailure(err)
	}
	d, err := s.matcher.Match(ctx, view)
	if err != nil {
		return learningFailure(err)
	}
	d, err = validateMatchDecision(snap.View, d)
	if err != nil {
		return learningFailure(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return learningFailure(err)
	}
	defer tx.Rollback()
	if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err != nil {
		return learningFailure(err)
	}
	if _, saved, err = s.readMatchTx(ctx, tx, job); err != nil || saved != nil {
		if err == nil {
			err = harness.ErrSkillConflict
		}
		return learningFailure(err)
	}
	match, err := planMatch(snap, d, s.now().UTC())
	if err != nil {
		return learningFailure(err)
	}
	data, _ = json.Marshal(matchResult{Decision: d, Match: match})
	if len(data) > 32<<10 {
		return learningFailure(invalid("matching result exceeds bounds"))
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_learning_matches SET result=?,result_sha=? WHERE id=? AND result_sha=''`, data, digest(data), snap.ID)
	if err != nil {
		return learningFailure(err)
	}
	if err = matchWriteApplied(result); err != nil {
		return learningFailure(err)
	}
	result, err = tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='processed',reason='' WHERE pattern_id=? AND revision=? AND state='queued' AND job_id=?`, snap.Pattern.ID, snap.Pattern.Revision, job.ID)
	if err != nil {
		return learningFailure(err)
	}
	if err = matchWriteApplied(result); err != nil {
		return learningFailure(err)
	}
	if err = tx.Commit(); err != nil {
		return learningFailure(err)
	}
	return "", false
}

func matchWriteApplied(result sql.Result) error {
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return harness.ErrSkillConflict
	}
	return err
}

func (s *Store) FinalizeMatchTx(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	state := "processed"
	if job.State != "completed" {
		state = "skipped"
	}
	_, err := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state=?,reason=? WHERE job_id=? AND state='queued'`, state, job.ErrorCode, job.ID)
	return err
}

func (s *Store) MatchingHooks() skillmaintenance.Hooks {
	return s.ExtractionHooks()
}

// Match history, like pattern history, is not a live promotion authority. It
// verifies saved decisions and projections without reading task trees or bodies.
func (s *Store) Matches(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperienceMatchPage, error) {
	page := harness.SkillExperienceMatchPage{Matches: []harness.SkillExperienceMatch{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	return s.matchPage(ctx, s.db, matchHistoryQuery+`
 WHERE m.session_id=? AND m.id>? AND m.result_sha<>'' AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_sharing sh WHERE sh.pattern_id=m.pattern_id)
 ORDER BY m.id LIMIT ?`, sessionID, after, limit, false)
}

const matchHistoryQuery = `SELECT m.id,m.pattern_id,m.revision,m.payload,m.payload_sha,m.result,m.result_sha,m.manual,
 p.current_revision,COALESCE(review.state_version,0),COALESCE(review.ignored,0),COALESCE(review.adjudication_revision,0),COALESCE(review.adjudication_id,'') FROM (
 SELECT id,session_id,pattern_id,revision,payload,payload_sha,result,result_sha,0 AS manual FROM df_learning_matches
 UNION ALL SELECT a.id,json_extract(CAST(a.payload AS TEXT),'$.matching.session') AS session_id,a.pattern_id,a.revision,a.payload,a.payload_sha,a.result,a.result_sha,1 AS manual FROM df_learning_adjudications a) m
 JOIN df_learning_patterns p ON p.id=m.pattern_id LEFT JOIN df_learning_pattern_reviews review ON review.pattern_id=p.id`

func (s *Store) WorkspaceMatches(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperienceMatchPage, error) {
	page := harness.SkillExperienceMatchPage{Matches: []harness.SkillExperienceMatch{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	owner, err := sessionWorkspaceTx(ctx, tx, sessionID)
	if err != nil {
		return page, err
	}
	var epoch int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch FROM harness_learning_controls WHERE session_id=?`, sessionID).Scan(&epoch); err != nil {
		return page, err
	}
	if err = s.sharedSessionTx(ctx, tx, sessionID, owner, epoch); err != nil {
		return page, err
	}
	page, err = s.matchPage(ctx, tx, matchHistoryQuery+`
 JOIN df_learning_pattern_sharing sh ON sh.pattern_id=m.pattern_id WHERE sh.workspace_key=? AND m.id>? AND m.result_sha<>'' ORDER BY m.id LIMIT ?`, owner, after, limit, true)
	if err != nil {
		return page, err
	}
	for _, match := range page.Matches {
		p, patternOwner, err := s.patternRecordTx(ctx, tx, match.PatternID, match.PatternRevision, false)
		if err != nil {
			return page, err
		}
		if !p.Shared || patternOwner != owner {
			return page, harness.ErrPermissionDenied
		}
		if err = s.sharedPatternTx(ctx, tx, p, owner); err != nil {
			return page, err
		}
	}
	return page, tx.Commit()
}

type matchHistoryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) matchPage(ctx context.Context, reader matchHistoryReader, query, key, after string, limit int, shared bool) (harness.SkillExperienceMatchPage, error) {
	page := harness.SkillExperienceMatchPage{Matches: []harness.SkillExperienceMatch{}}
	rows, err := reader.QueryContext(ctx, query, key, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, pattern, sha, resultSHA string
		var revision int64
		var manual bool
		var review harness.SkillLearningPatternReview
		var data, output []byte
		if err = rows.Scan(&id, &pattern, &revision, &data, &sha, &output, &resultSHA, &manual,
			&review.PatternRevision, &review.StateVersion, &review.Ignored, &review.AdjudicationRevision, &review.AdjudicationID); err != nil {
			return page, err
		}
		if len(page.Matches) == limit {
			page.Next = page.Matches[len(page.Matches)-1].ID
			break
		}
		var snap matchSnapshot
		var result matchResult
		if manual {
			adjudication, saved, readErr := adjudicationRecord(data, sha, output, resultSHA, id, pattern, revision)
			if readErr != nil {
				return page, readErr
			}
			snap, result = adjudication.Matching, saved
		} else if len(data) > (2<<20)+(64<<10) || len(output) > 32<<10 || digest(data) != sha || digest(output) != resultSHA || json.Unmarshal(data, &snap) != nil || json.Unmarshal(output, &result) != nil {
			return page, invalid("inconsistent matching history")
		}
		if snap.ID != id || !shared && snap.Session != key || shared && snap.Owner != key || snap.Pattern.ID != pattern || snap.Pattern.Revision != revision || len(snap.Choices) > 8 || len(snap.Choices) != len(snap.View.Skills) || result.Match.CreatedAt.IsZero() {
			return page, invalid("inconsistent matching history")
		}
		d, err := validateMatchDecision(snap.View, result.Decision)
		if err != nil || encodedDigest(d) != encodedDigest(result.Decision) {
			return page, invalid("inconsistent matching history decision")
		}
		expected, err := planMatch(snap, d, result.Match.CreatedAt)
		if manual {
			expected.Origin, expected.SourceState = "host_adjudicated", "checked_at_adjudication"
		}
		if err != nil || encodedDigest(expected) != encodedDigest(result.Match) {
			return page, invalid("inconsistent matching history projection")
		}
		result.Match.SourceState = "not_revalidated"
		review.PatternID = pattern
		result.Match.Review = &review
		page.Matches = append(page.Matches, result.Match)
	}
	return page, errors.Join(rows.Err(), rows.Close())
}
