package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const AdjudicationPolicyVersion = "procedural-host-adjudication-v1"

type adjudicationSnapshot struct {
	Request      harness.SkillLearningPatternAdjudicationRequest `json:"request"`
	Matching     matchSnapshot                                   `json:"matching"`
	Extractor    string                                          `json:"extractor"`
	Consolidator string                                          `json:"consolidator"`
	ActorSession string                                          `json:"actor_session"`
	RequestSHA   string                                          `json:"request_sha256"`
}

func validReviewRequest(pattern, scope, key string, revision, version int64) bool {
	return validText(pattern, 128, true) && validText(key, 128, true) && strings.TrimSpace(key) == key &&
		(scope == "session" || scope == "workspace") && revision > 0 && version >= 0 && version < math.MaxInt64
}

// The authenticated host chooses an explicit scope. Owning a session or sharing
// a directory alone cannot authorize control of another conversation's pattern.
func (s *Store) reviewScopeTx(ctx context.Context, tx *sql.Tx, session, pattern, scope string, revision int64) (patternSnapshot, error) {
	p, owner, err := s.patternRecordTx(ctx, tx, pattern, revision, false)
	if errors.Is(err, sql.ErrNoRows) {
		err = harness.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if scope == "session" {
		if p.Shared || p.Pattern.SessionID != session {
			return p, harness.ErrPermissionDenied
		}
		return p, nil
	}
	var epoch int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch FROM harness_learning_controls WHERE session_id=?`, session).Scan(&epoch); err != nil {
		return p, err
	}
	if !p.Shared {
		return p, harness.ErrPermissionDenied
	}
	if err = s.sharedSessionTx(ctx, tx, session, owner, epoch); err == nil {
		err = s.sharedPatternTx(ctx, tx, p, owner)
	}
	return p, err
}

// Receipts are scoped to a revocable pattern and contain no request text. Replay
// returns the original outcome, not a promise that the current review is equal.
func readReviewReceiptTx(ctx context.Context, tx *sql.Tx, session, key, requestSHA string) (*harness.SkillLearningPatternReviewResult, error) {
	var data []byte
	var sha, resultSHA string
	err := tx.QueryRowContext(ctx, `SELECT request_sha,result,result_sha FROM df_learning_review_receipts WHERE session_id=? AND request_key=?`, session, key).Scan(&sha, &data, &resultSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sha != requestSHA {
		return nil, harness.ErrSkillConflict
	}
	var result harness.SkillLearningPatternReviewResult
	if len(data) > 40<<10 || digest(data) != resultSHA || json.Unmarshal(data, &result) != nil || result.Review.StateVersion < 1 {
		return nil, invalid("inconsistent review receipt")
	}
	return &result, nil
}

func writeReviewReceiptTx(ctx context.Context, tx *sql.Tx, session, key, requestSHA string, result harness.SkillLearningPatternReviewResult) error {
	data, err := json.Marshal(result)
	if err != nil || len(data) > 40<<10 {
		return invalid("review receipt exceeds bounds")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO df_learning_review_receipts VALUES(?,?,?,?,?,?)`, session, key, result.Review.PatternID, requestSHA, data, digest(data))
	return err
}

func writePatternReviewTx(ctx context.Context, tx *sql.Tx, review harness.SkillLearningPatternReview) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_pattern_reviews VALUES(?,?,?,?,?) ON CONFLICT(pattern_id) DO UPDATE SET
 state_version=excluded.state_version,ignored=excluded.ignored,adjudication_revision=excluded.adjudication_revision,adjudication_id=excluded.adjudication_id`,
		review.PatternID, review.StateVersion, review.Ignored, review.AdjudicationRevision, review.AdjudicationID)
	return err
}

func (s *Store) SetPatternIgnored(ctx context.Context, session string, request harness.SkillLearningPatternIgnoreRequest) (harness.SkillLearningPatternReviewResult, error) {
	var result harness.SkillLearningPatternReviewResult
	if !validReviewRequest(request.PatternID, request.Scope, request.RequestKey, request.ExpectedPatternRevision, request.ExpectedStateVersion) {
		return result, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = s.reviewScopeTx(ctx, tx, session, request.PatternID, request.Scope, request.ExpectedPatternRevision); err != nil {
		return result, err
	}
	requestSHA := encodedDigest([]any{"pattern.ignore", request})
	if saved, err := readReviewReceiptTx(ctx, tx, session, request.RequestKey, requestSHA); err != nil || saved != nil {
		if saved != nil {
			if saved.Match != nil || saved.Review.PatternID != request.PatternID || saved.Review.PatternRevision != request.ExpectedPatternRevision ||
				saved.Review.StateVersion != request.ExpectedStateVersion+1 || saved.Review.Ignored != request.Ignored ||
				saved.Review.AdjudicationID != "" || saved.Review.AdjudicationRevision != 0 {
				return result, invalid("inconsistent ignore receipt")
			}
			result = *saved
		}
		return result, err
	}
	review, err := patternReviewTx(ctx, tx, request.PatternID)
	if err != nil {
		return result, err
	}
	if review.PatternRevision != request.ExpectedPatternRevision || review.StateVersion != request.ExpectedStateVersion || review.Ignored == request.Ignored {
		return result, harness.ErrSkillConflict
	}
	if err = forgetPromotionsTx(ctx, tx, request.PatternID); err != nil {
		return result, err
	}
	if request.Ignored {
		if err = cancelPatternMatchingTx(ctx, tx, request.PatternID); err != nil {
			return result, err
		}
	}
	review.StateVersion++
	review.Ignored = request.Ignored
	// An old manual decision never silently becomes current again on restore.
	review.AdjudicationID, review.AdjudicationRevision = "", 0
	result.Review = review
	if err = writePatternReviewTx(ctx, tx, review); err == nil {
		err = writeReviewReceiptTx(ctx, tx, session, request.RequestKey, requestSHA, result)
	}
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func cancelPatternMatchingTx(ctx context.Context, tx *sql.Tx, pattern string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,
 token='',lease_ns=0,retry_ns=0,error_code='cancelled' WHERE state IN ('queued','retry','running')
 AND id IN (SELECT job_id FROM df_learning_matches WHERE pattern_id=?)`, pattern); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE df_learning_match_inputs SET state='cancelled',reason='host_review'
 WHERE pattern_id=? AND state IN ('pending','queued')`, pattern)
	return err
}

func (s *Store) AdjudicatePattern(ctx context.Context, session string, request harness.SkillLearningPatternAdjudicationRequest) (harness.SkillLearningPatternReviewResult, error) {
	var result harness.SkillLearningPatternReviewResult
	if !validReviewRequest(request.PatternID, request.Scope, request.RequestKey, request.ExpectedPatternRevision, request.ExpectedStateVersion) ||
		request.Relation != "body_gap" && request.Relation != "trigger_mismatch" && request.Relation != "already_covered" ||
		!validText(request.Reason, 2000, true) || !harness.ValidSkillName(request.Target.Name) || !request.ExpectedHead.Exists || !request.ExpectedHead.Enabled {
		return result, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	ctx = withEvidenceCache(ctx, tx)
	if _, err = s.reviewScopeTx(ctx, tx, session, request.PatternID, request.Scope, request.ExpectedPatternRevision); err != nil {
		return result, err
	}
	requestSHA := encodedDigest([]any{"pattern.adjudicate", request})
	if saved, err := readReviewReceiptTx(ctx, tx, session, request.RequestKey, requestSHA); err != nil || saved != nil {
		if saved != nil {
			if err = validateAdjudicationReceiptTx(ctx, tx, session, request, requestSHA, *saved); err != nil {
				return result, err
			}
			result = *saved
		}
		return result, err
	}
	if !s.PromotionEnabled() {
		return result, harness.ErrPermissionDenied
	}
	review, err := patternReviewTx(ctx, tx, request.PatternID)
	if err != nil {
		return result, err
	}
	if review.Ignored || review.PatternRevision != request.ExpectedPatternRevision || review.StateVersion != request.ExpectedStateVersion {
		return result, harness.ErrSkillConflict
	}
	p, err := s.patternTx(ctx, tx, request.PatternID, request.ExpectedPatternRevision, true)
	if err != nil {
		return result, err
	}
	if p.Pattern.EvidenceState == "conflicting" || p.Pattern.EvidenceState == "ambiguous" || p.Pattern.EvidenceState == "unresolved" {
		return result, harness.ErrPermissionDenied
	}
	if err = s.guardPatternConsolidationTx(ctx, tx, p); err != nil {
		return result, err
	}
	inventory, choices, view, err := s.matchTargetViewTx(ctx, tx, p, &request.Target)
	if err != nil {
		return result, err
	}
	choice := choices[0]
	if choice.Skill.Target != request.Target || choice.Skill.Head != request.ExpectedHead || !skills.SameOverlayBase(choice.Skill.OverlayBase, request.OverlayBase) {
		return result, harness.ErrSkillConflict
	}
	viewBytes, _ := json.Marshal(view)
	if len(viewBytes) > s.matchInputLimit() {
		return result, invalid("adjudication input exceeds bounds")
	}
	d, err := validateMatchDecision(view, harness.SkillExperienceMatchDecision{Relation: request.Relation, SkillID: choice.ID, Reason: request.Reason})
	if err != nil {
		return result, err
	}
	request.Reason = d.Reason
	now := s.now().UTC()
	snap := adjudicationSnapshot{Request: request, Extractor: s.extractor.Identity(), Consolidator: s.consolidator.Identity(), ActorSession: session, RequestSHA: requestSHA,
		Matching: matchSnapshot{Session: p.Pattern.SessionID,
			Pattern: patternDependency{ID: request.PatternID, Revision: request.ExpectedPatternRevision, SHA: encodedDigest(p)},
			Catalog: inventory.Catalog, InventorySHA: encodedDigest(inventory), Choices: choices, View: view}}
	if err = tx.QueryRowContext(ctx, `SELECT owner_key FROM df_learning_patterns WHERE id=?`, request.PatternID).Scan(&snap.Matching.Owner); err != nil {
		return result, err
	}
	snap.Matching.ID = "ladj_" + encodedDigest([]any{AdjudicationPolicyVersion, snap})
	match, err := planAdjudication(snap, d, now)
	if err != nil {
		return result, err
	}
	data, _ := json.Marshal(snap)
	output, _ := json.Marshal(matchResult{Decision: d, Match: match})
	if len(data) > s.matchInputLimit()+(64<<10) || len(output) > 32<<10 {
		return result, invalid("adjudication exceeds bounds")
	}
	if err = forgetPromotionsTx(ctx, tx, request.PatternID); err == nil {
		err = cancelPatternMatchingTx(ctx, tx, request.PatternID)
	}
	if err != nil {
		return result, err
	}
	review.StateVersion++
	review.AdjudicationID, review.AdjudicationRevision = match.ID, match.PatternRevision
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_adjudications VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, match.ID, session, request.PatternID,
		match.PatternRevision, review.StateVersion, AdjudicationPolicyVersion, s.matcher.Identity(), s.matchAccess.Identity(),
		data, digest(data), output, digest(output), now.Format(time.RFC3339Nano)); err != nil {
		return result, err
	}
	result = harness.SkillLearningPatternReviewResult{Review: review, Match: &match}
	if err = writePatternReviewTx(ctx, tx, review); err == nil {
		err = writeReviewReceiptTx(ctx, tx, session, request.RequestKey, requestSHA, result)
	}
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func validateAdjudicationReceiptTx(ctx context.Context, tx *sql.Tx, session string, request harness.SkillLearningPatternAdjudicationRequest, requestSHA string, saved harness.SkillLearningPatternReviewResult) error {
	if saved.Match == nil || saved.Review.PatternID != request.PatternID || saved.Review.PatternRevision != request.ExpectedPatternRevision ||
		saved.Review.StateVersion != request.ExpectedStateVersion+1 || saved.Review.Ignored || saved.Review.AdjudicationRevision != request.ExpectedPatternRevision ||
		saved.Review.AdjudicationID != saved.Match.ID {
		return invalid("inconsistent adjudication receipt")
	}
	var data, output []byte
	var sha, resultSHA, actor string
	if err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result,result_sha,session_id FROM df_learning_adjudications WHERE id=?`, saved.Match.ID).Scan(
		&data, &sha, &output, &resultSHA, &actor); err != nil {
		return err
	}
	snap, result, err := adjudicationRecord(data, sha, output, resultSHA, saved.Match.ID, request.PatternID, request.ExpectedPatternRevision)
	if err != nil {
		return err
	}
	if actor != session || snap.ActorSession != session || snap.RequestSHA != requestSHA || snap.Request.ExpectedStateVersion != request.ExpectedStateVersion ||
		encodedDigest(result.Match) != encodedDigest(*saved.Match) {
		return invalid("adjudication receipt differs from original fixed result")
	}
	return nil
}

func planAdjudication(snap adjudicationSnapshot, d harness.SkillExperienceMatchDecision, now time.Time) (harness.SkillExperienceMatch, error) {
	m, err := planMatch(snap.Matching, d, now)
	m.Origin, m.SourceState = "host_adjudicated", "checked_at_adjudication"
	return m, err
}

func (s *Store) guardPatternConsolidationTx(ctx context.Context, tx *sql.Tx, p patternSnapshot) error {
	var jobID string
	if err := tx.QueryRowContext(ctx, `SELECT c.job_id FROM df_learning_pattern_revisions r JOIN df_learning_consolidations c ON c.id=r.consolidation_id
 WHERE r.pattern_id=? AND r.revision=?`, p.Pattern.ID, p.Pattern.Revision).Scan(&jobID); err != nil {
		return err
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if job.State != "completed" {
		return harness.ErrPermissionDenied
	}
	_, result, err := s.readConsolidationTx(ctx, tx, job)
	if err == nil && result == nil {
		err = harness.ErrPermissionDenied
	}
	return err
}

func (s *Store) readAdjudicationTx(ctx context.Context, tx *sql.Tx, id string) (out matchSnapshot, saved *matchResult, inputSHA string, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	var data, output []byte
	var sha, resultSHA, policy, model, access, pattern, session string
	var revision, version int64
	err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha,result,result_sha,policy_version,model_identity,access_identity,
 pattern_id,revision,state_version,session_id FROM df_learning_adjudications WHERE id=?`, id).Scan(
		&data, &sha, &output, &resultSHA, &policy, &model, &access, &pattern, &revision, &version, &session)
	if err != nil {
		return matchSnapshot{}, nil, "", err
	}
	snap, result, err := adjudicationRecord(data, sha, output, resultSHA, id, pattern, revision)
	if err != nil {
		return snap.Matching, nil, "", err
	}
	if snap.ActorSession != session || snap.Request.ExpectedStateVersion+1 != version {
		return snap.Matching, nil, "", invalid("inconsistent adjudication actor or control version")
	}
	if !s.PromotionEnabled() || policy != AdjudicationPolicyVersion || model != s.matcher.Identity() || access != s.matchAccess.Identity() ||
		snap.Extractor != s.extractor.Identity() || snap.Consolidator != s.consolidator.Identity() {
		return snap.Matching, nil, "", harness.ErrPermissionDenied
	}
	review, err := patternReviewTx(ctx, tx, pattern)
	if err != nil {
		return snap.Matching, nil, "", err
	}
	if review.Ignored || review.StateVersion != version || review.AdjudicationID != id || review.AdjudicationRevision != revision {
		return snap.Matching, nil, "", harness.ErrSkillConflict
	}
	if _, err = s.reviewScopeTx(ctx, tx, session, pattern, snap.Request.Scope, revision); err != nil {
		return snap.Matching, nil, "", err
	}
	p, err := s.patternTx(ctx, tx, pattern, revision, true)
	if err != nil {
		return snap.Matching, nil, "", err
	}
	if encodedDigest(p) != snap.Matching.Pattern.SHA {
		return snap.Matching, nil, "", harness.ErrSkillConflict
	}
	inventory, choices, view, err := s.matchTargetViewTx(ctx, tx, p, &snap.Request.Target)
	if err != nil {
		return snap.Matching, nil, "", err
	}
	if inventory.Catalog != snap.Matching.Catalog || encodedDigest(inventory) != snap.Matching.InventorySHA ||
		encodedDigest(choices) != encodedDigest(snap.Matching.Choices) || encodedDigest(view) != encodedDigest(snap.Matching.View) {
		return snap.Matching, nil, "", harness.ErrSkillConflict
	}
	return snap.Matching, &result, sha, nil
}

func adjudicationRecord(data []byte, sha string, output []byte, resultSHA, id, pattern string, revision int64) (adjudicationSnapshot, matchResult, error) {
	var snap adjudicationSnapshot
	var result matchResult
	if len(data) > (2<<20)+(64<<10) || len(output) > 32<<10 || digest(data) != sha || digest(output) != resultSHA ||
		json.Unmarshal(data, &snap) != nil || json.Unmarshal(output, &result) != nil || snap.Matching.ID != id ||
		snap.Matching.Pattern.ID != pattern || snap.Matching.Pattern.Revision != revision || snap.Request.PatternID != pattern ||
		snap.Request.ExpectedPatternRevision != revision || result.Match.CreatedAt.IsZero() || len(snap.Matching.Choices) < 1 ||
		len(snap.Matching.Choices) > 8 || len(snap.Matching.Choices) != len(snap.Matching.View.Skills) {
		return snap, result, invalid("inconsistent adjudication record")
	}
	identity := snap
	identity.Matching.ID = ""
	if !validReviewRequest(pattern, snap.Request.Scope, snap.Request.RequestKey, revision, snap.Request.ExpectedStateVersion) ||
		!validText(snap.ActorSession, 256, true) || len(snap.RequestSHA) != 64 || "ladj_"+encodedDigest([]any{AdjudicationPolicyVersion, identity}) != id {
		return snap, result, invalid("inconsistent adjudication identity")
	}
	d, err := validateMatchDecision(snap.Matching.View, result.Decision)
	if err != nil || encodedDigest(d) != encodedDigest(result.Decision) || d.Relation != snap.Request.Relation ||
		d.Reason != snap.Request.Reason || d.SuggestedName != "" || d.SkillID != snap.Matching.Choices[0].ID ||
		!slices.Contains([]string{"body_gap", "trigger_mismatch", "already_covered"}, d.Relation) {
		return snap, result, invalid("inconsistent adjudication decision")
	}
	expected, err := planAdjudication(snap, d, result.Match.CreatedAt)
	if err != nil || encodedDigest(expected) != encodedDigest(result.Match) || expected.Target == nil || expected.Head == nil ||
		*expected.Target != snap.Request.Target || *expected.Head != snap.Request.ExpectedHead || !skills.SameOverlayBase(expected.OverlayBase, snap.Request.OverlayBase) {
		return snap, result, invalid("inconsistent adjudication projection")
	}
	return snap, result, nil
}
