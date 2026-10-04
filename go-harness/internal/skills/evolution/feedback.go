package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxFeedbackRevisions = 1000

func migrateFeedback(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_feedback (
 signal_id TEXT NOT NULL REFERENCES df_skill_signals(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,request_key TEXT NOT NULL,request_sha TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,
 PRIMARY KEY(signal_id,revision),UNIQUE(signal_id,request_key));
CREATE TRIGGER IF NOT EXISTS df_skill_feedback_created AFTER INSERT ON df_skill_feedback
 BEGIN UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_feedback_deleted AFTER DELETE ON df_skill_feedback
 BEGIN UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1; END;`)
	return err
}

func validSignalID(id string) bool {
	return strings.HasPrefix(id, "sig_") && hexDigest(strings.TrimPrefix(id, "sig_"))
}

func validFeedbackReads(ids []int64, withdrawn bool) bool {
	if withdrawn {
		return len(ids) == 0
	}
	if len(ids) < 1 || len(ids) > 16 {
		return false
	}
	var last int64
	for _, id := range ids {
		if id <= last {
			return false
		}
		last = id
	}
	return true
}

func readFeedback(ctx context.Context, tx *sql.Tx, signal harness.SkillSignal, revision int64) (harness.SkillFeedback, error) {
	var feedback harness.SkillFeedback
	var payload []byte
	var digest, requestSHA string
	err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha,request_sha FROM df_skill_feedback WHERE signal_id=? AND revision=?`, signal.ID, revision).Scan(&payload, &digest, &requestSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return feedback, harness.ErrNotFound
	}
	if err != nil {
		return feedback, err
	}
	if len(payload) > 32<<10 || hash(payload) != digest || !hexDigest(requestSHA) || json.Unmarshal(payload, &feedback) != nil ||
		feedback.SignalID != signal.ID || feedback.Revision != revision || revision < 1 || revision > maxFeedbackRevisions ||
		feedback.Target != signal.Target || feedback.SkillVersion != signal.Version || feedback.RootID != signal.RootID || feedback.RootSessionID != signal.RootSessionID ||
		feedback.SignalStateVersion < 1 || feedback.SignalStateVersion > signal.StateVersion || feedback.Origin != "operator" || !feedbackKind(feedback.Kind) || !validFeedbackReads(feedback.ReadIDs, feedback.Kind == "withdrawn") {
		return harness.SkillFeedback{}, invalid("feedback record is inconsistent")
	}
	summary, _, err := cleanFeedbackSummary(feedback.Summary)
	if err != nil || summary != feedback.Summary || (feedback.Kind == "withdrawn") != (feedback.Summary == "") {
		return harness.SkillFeedback{}, invalid("feedback summary is inconsistent")
	}
	if _, err := time.Parse(time.RFC3339Nano, feedback.CreatedAt); err != nil {
		return harness.SkillFeedback{}, invalid("feedback timestamp is inconsistent")
	}
	return feedback, nil
}

// SubmitFeedback replaces one root/version judgment without adding a vote. It
// uses only existing linked reads. Authorization precedes request-key replay;
// replay precedes new-write CAS, so a lost reply survives later evidence/reviews.
func (s *Service) SubmitFeedback(ctx context.Context, request harness.SkillFeedbackRequest) (harness.SkillFeedbackResult, error) {
	var result harness.SkillFeedbackResult
	if !validSignalID(request.SignalID) || request.ExpectedSignalStateVersion < 1 || request.ExpectedFeedbackVersion < 0 ||
		!feedbackRequestKey(request.RequestKey) || !feedbackKind(request.Kind) {
		return result, invalid("feedback requires identity, versions, request key and kind")
	}
	request.ReadIDs = slices.Clone(request.ReadIDs)
	slices.Sort(request.ReadIDs)
	if !validFeedbackReads(request.ReadIDs, request.Kind == "withdrawn") {
		return result, invalid("feedback requires 1..16 distinct read references, or none for withdrawal")
	}
	summary, redacted, err := cleanFeedbackSummary(request.Summary)
	if err != nil {
		return result, err
	}
	request.Summary = strings.TrimSpace(request.Summary)
	if (request.Kind == "withdrawn") != (summary == "") {
		return result, invalid("feedback requires a summary, or empty text for withdrawal")
	}
	if request.ReadIDs == nil {
		request.ReadIDs = []int64{}
	}
	requestSHA := encodedHash(request) // Hash only; raw summary is not retained.
	allowed, _, _, _, err := s.querySources(harness.SkillQuery{Limit: 1}, "skill-feedback-v1")
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	signal, err := readSignal(ctx, tx, request.SignalID, allowed)
	if err != nil {
		return result, err
	}
	result.CurrentRevision = signal.FeedbackRevision
	var revision int64
	var priorSHA string
	err = tx.QueryRowContext(ctx, `SELECT revision,request_sha FROM df_skill_feedback WHERE signal_id=? AND request_key=?`, signal.ID, request.RequestKey).Scan(&revision, &priorSHA)
	if err == nil {
		if priorSHA != requestSHA {
			return result, harness.ErrSkillConflict
		}
		result.Feedback, err = readFeedback(ctx, tx, signal, revision)
		if err != nil {
			return result, err
		}
		result.Replayed = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if signal.StateVersion != request.ExpectedSignalStateVersion || signal.FeedbackRevision != request.ExpectedFeedbackVersion {
		return result, harness.ErrSkillConflict
	}
	if signal.FeedbackRevision >= maxFeedbackRevisions || signal.FeedbackRevision >= maxFeedbackRevisions-1 && request.Kind != "withdrawn" {
		return result, harness.ErrSkillFeedbackLimit
	}
	if request.Kind == "withdrawn" && signal.FeedbackRevision == 0 {
		return result, invalid("there is no feedback to withdraw")
	}
	for _, id := range request.ReadIDs {
		var found bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_signal_reads l
 JOIN harness_skill_reads r ON r.id=l.read_id JOIN harness_skill_outcomes o ON o.sequence=l.outcome_seq
 WHERE l.signal_id=? AND r.id=? AND r.root_id=? AND r.source_key=? AND r.source_id=? AND r.scope=? AND r.name=? AND r.version=?
 AND o.run_id=r.run_id AND o.session_id=r.session_id)`, signal.ID, id, signal.RootID, signal.Target.SourceIdentity, signal.Target.SourceID, signal.Target.Scope, signal.Target.Name, signal.Version).Scan(&found)
		if err != nil {
			return result, err
		}
		if !found {
			return result, invalid("feedback read does not belong to the observed signal")
		}
	}
	feedback := harness.SkillFeedback{
		SignalID: signal.ID, Revision: signal.FeedbackRevision + 1, Target: signal.Target,
		SkillVersion: signal.Version, RootID: signal.RootID, RootSessionID: signal.RootSessionID,
		SignalStateVersion: signal.StateVersion, Origin: "operator", Kind: request.Kind,
		Summary: summary, SummaryRedacted: redacted, ReadIDs: request.ReadIDs, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(feedback)
	if err != nil {
		return result, err
	}
	if len(payload) > 32<<10 {
		return result, invalid("encoded feedback exceeds management bounds")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_feedback(signal_id,revision,request_key,request_sha,payload,payload_sha) VALUES(?,?,?,?,?,?)`, signal.ID, feedback.Revision, request.RequestKey, requestSHA, payload, hash(payload)); err != nil {
		return result, err
	}
	result.Feedback, result.CurrentRevision = feedback, feedback.Revision
	return result, tx.Commit()
}

func (s *Service) Feedback(ctx context.Context, id string) (harness.SkillFeedbackSnapshot, error) {
	var result harness.SkillFeedbackSnapshot
	if !validSignalID(id) {
		return result, invalid("signal id is invalid")
	}
	allowed, _, _, _, err := s.querySources(harness.SkillQuery{Limit: 1}, "skill-feedback-v1")
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.Signal, err = readSignal(ctx, tx, id, allowed)
	if err != nil {
		return result, err
	}
	if result.Signal.FeedbackRevision > 0 {
		feedback, err := readFeedback(ctx, tx, result.Signal, result.Signal.FeedbackRevision)
		if err != nil {
			return result, err
		}
		result.Feedback = &feedback
		result.EvidenceCurrent = feedback.Kind != "withdrawn" && feedback.SignalStateVersion == result.Signal.StateVersion
	}
	return result, tx.Commit()
}

func (s *Service) QueryFeedback(ctx context.Context, id, after string, limit int) (harness.SkillFeedbackPage, error) {
	page := harness.SkillFeedbackPage{Revisions: []harness.SkillFeedback{}}
	if !validSignalID(id) {
		return page, invalid("signal id is invalid")
	}
	allowed, _, _, cursor, err := s.querySources(harness.SkillQuery{Name: id, Cursor: after, Limit: limit}, "skill-feedback-history-v1")
	if err != nil {
		return page, err
	}
	var last int64
	if cursor.Key != "" {
		last, err = strconv.ParseInt(cursor.Key, 10, 64)
		if err != nil || last < 1 || last >= maxFeedbackRevisions {
			return page, invalid("feedback cursor is invalid")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	signal, err := readSignal(ctx, tx, id, allowed)
	if err != nil {
		return page, err
	}
	page.CurrentRevision = signal.FeedbackRevision
	if after != "" && cursor.Revision != page.CurrentRevision {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.CurrentRevision
	for revision := last + 1; revision <= page.CurrentRevision; revision++ {
		if len(page.Revisions) == limit {
			page.Next = encodeCursor(cursor)
			break
		}
		feedback, err := readFeedback(ctx, tx, signal, revision)
		if err != nil {
			return page, err
		}
		page.Revisions = append(page.Revisions, feedback)
		cursor.Key = strconv.FormatInt(revision, 10)
	}
	return page, tx.Commit()
}
