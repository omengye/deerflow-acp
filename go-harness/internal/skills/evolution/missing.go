package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SubmitMissingReport records a bounded operator judgment, even for a run with
// no skill reads. It never assesses absence/causality, changes a run, or starts
// generation. Root ownership is checked before idempotent replay, then new CAS.
func (s *Service) SubmitMissingReport(ctx context.Context, request harness.SkillMissingReportRequest) (harness.SkillMissingReportResult, error) {
	var result harness.SkillMissingReportResult
	if !validMissingIdentity(request.SessionID) || !validMissingIdentity(request.RunID) || request.ExpectedReportVersion < 0 || !feedbackRequestKey(request.RequestKey) || !missingKind(request.Kind) {
		return result, invalid("missing report requires session, run, version, request key and kind")
	}
	summary, redacted, err := cleanFeedbackSummary(request.Summary)
	if err != nil {
		return result, err
	}
	if (request.Kind == "withdrawn") != (summary == "") {
		return result, invalid("missing report requires a summary, or empty text for withdrawal")
	}
	request.Summary = strings.TrimSpace(request.Summary)
	requestSHA := encodedHash(request) // Original intent digest only; raw text is not retained.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// Also serialize before the read on a SQLite connection without _txlock=immediate.
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_missing_catalog SET revision=revision WHERE id=1`); err != nil {
		return result, err
	}
	rootID, err := missingRoot(ctx, tx, request.SessionID, request.RunID)
	if err != nil {
		return result, err
	}
	result.CurrentRevision, err = missingRevision(ctx, tx, rootID, request.SessionID)
	if err != nil {
		return result, err
	}
	var revision int64
	var priorSHA string
	err = tx.QueryRowContext(ctx, `SELECT revision,request_sha FROM df_skill_missing_revisions WHERE root_id=? AND request_key=?`, rootID, request.RequestKey).Scan(&revision, &priorSHA)
	if err == nil {
		if priorSHA != requestSHA {
			return result, harness.ErrSkillConflict
		}
		result.Report, err = readMissingReport(ctx, tx, rootID, request.SessionID, revision)
		if err != nil {
			return result, err
		}
		result.Replayed = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if result.CurrentRevision != request.ExpectedReportVersion {
		return result, harness.ErrSkillConflict
	}
	if result.CurrentRevision >= maxMissingReportRevisions || result.CurrentRevision >= maxMissingReportRevisions-1 && request.Kind != "withdrawn" {
		return result, harness.ErrSkillMissingReportLimit
	}
	if request.Kind == "withdrawn" && result.CurrentRevision == 0 {
		return result, invalid("there is no missing report to withdraw")
	}
	report := harness.SkillMissingReport{ID: missingReportID(rootID), RootID: rootID, RootSessionID: request.SessionID, RunID: request.RunID,
		Revision: result.CurrentRevision + 1, Origin: "operator", Kind: request.Kind, Summary: summary, SummaryRedacted: redacted,
		Attribution: "unassessed", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	payload, err := json.Marshal(report)
	if err != nil {
		return result, err
	}
	if len(payload) > 32<<10 {
		return result, invalid("encoded missing report exceeds management bounds")
	}
	if result.CurrentRevision == 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_missing_reports(root_id,id,session_id) VALUES(?,?,?)`, rootID, report.ID, request.SessionID); err != nil {
			return result, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_missing_revisions(root_id,revision,run_id,request_key,request_sha,payload,payload_sha) VALUES(?,?,?,?,?,?,?)`, rootID, report.Revision, request.RunID, request.RequestKey, requestSHA, payload, hash(payload)); err != nil {
		return result, err
	}
	result.Report, result.CurrentRevision = report, report.Revision
	return result, tx.Commit()
}

func (s *Service) MissingReport(ctx context.Context, sessionID, runID string) (harness.SkillMissingReportSnapshot, error) {
	var snapshot harness.SkillMissingReportSnapshot
	if !validMissingIdentity(sessionID) || !validMissingIdentity(runID) {
		return snapshot, invalid("missing report requires session and run identities")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	rootID, err := missingRoot(ctx, tx, sessionID, runID)
	if err != nil {
		return snapshot, err
	}
	snapshot.RootID, snapshot.RootSessionID = rootID, sessionID
	revision, err := missingRevision(ctx, tx, rootID, sessionID)
	if err != nil {
		return snapshot, err
	}
	if revision > 0 {
		report, err := readMissingReport(ctx, tx, rootID, sessionID, revision)
		if err != nil {
			return snapshot, err
		}
		snapshot.Report = &report
	}
	return snapshot, tx.Commit()
}
