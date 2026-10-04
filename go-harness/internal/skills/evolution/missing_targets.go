package evolution

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type missingTargetCursor struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
	RootID    string `json:"root_id"`
}

func encodeMissingTargetCursor(cursor missingTargetCursor) string {
	data, _ := json.Marshal(cursor) // This fixed structure contains only strings.
	return base64.RawURLEncoding.EncodeToString(data)
}

func missingTargetTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.UTC().Format(time.RFC3339Nano) == value
}

func decodeMissingTargetCursor(after, sessionID string) (missingTargetCursor, error) {
	cursor := missingTargetCursor{Kind: "skill-missing-targets-v1", SessionID: sessionID}
	if after == "" {
		return cursor, nil
	}
	var saved missingTargetCursor
	data, err := base64.RawURLEncoding.Strict().DecodeString(after)
	// Accept only our canonical encoding. This also rejects unknown, missing,
	// duplicate or reordered fields, trailing JSON, whitespace and base64 aliases.
	if err != nil || json.Unmarshal(data, &saved) != nil || encodeMissingTargetCursor(saved) != after ||
		saved.Kind != cursor.Kind || saved.SessionID != sessionID || !validMissingIdentity(saved.RootID) || !missingTargetTime(saved.CreatedAt) {
		return cursor, invalid("missing report target cursor does not match query")
	}
	return saved, nil
}

func missingTargetStatus(status string) bool {
	switch status {
	case "pending", "running", "suspending", "waiting_input", "resuming", "completed", "failed", "cancelled", "needs_reconciliation":
		return true
	default:
		return false
	}
}

// QueryMissingReportTargets discovers real root tasks even when no skill was
// read and no report exists. This is a live keyset list, not a frozen snapshot:
// newer tasks appear on refresh, while status/report changes keep their place.
// The cursor anchor must still exist and retain its original creation time.
func (s *Service) QueryMissingReportTargets(ctx context.Context, sessionID, after string, limit int) (harness.SkillMissingReportTargetPage, error) {
	page := harness.SkillMissingReportTargetPage{Targets: []harness.SkillMissingReportTarget{}}
	if !validMissingIdentity(sessionID) || len(after) > 2048 || limit < 1 || limit > 100 {
		return page, invalid("missing report target query is invalid")
	}
	cursor, err := decodeMissingTargetCursor(after, sessionID)
	if err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = missingSession(ctx, tx, sessionID); err != nil {
		return page, err
	}
	if after != "" {
		var runID, created string
		err = tx.QueryRowContext(ctx, `SELECT r.id,r.created_at FROM budget_roots b
 JOIN harness_runs r ON r.id=b.root_run_id AND r.session_id=b.session_id
 WHERE b.id=? AND b.session_id=?`, cursor.RootID, sessionID).Scan(&runID, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return page, harness.ErrSkillConflict
		}
		if err != nil {
			return page, err
		}
		root, err := missingRoot(ctx, tx, sessionID, runID)
		if err != nil {
			return page, err
		}
		if root != cursor.RootID || created != cursor.CreatedAt {
			return page, harness.ErrSkillConflict
		}
	}
	// Runtime timestamps are canonical UTC RFC3339Nano. Removing the final Z
	// makes variable fractional precision sort chronologically (.1 < .12).
	// Keeping Z would reverse the order whenever one fraction prefixes another.
	rows, err := tx.QueryContext(ctx, `SELECT b.id,b.root_run_id,r.created_at,r.status
 FROM budget_roots b JOIN harness_runs r ON r.id=b.root_run_id AND r.session_id=b.session_id
 WHERE b.session_id=? AND (?='' OR rtrim(r.created_at,'Z')<rtrim(?,'Z')
 OR (r.created_at=? AND b.id<?))
 ORDER BY rtrim(r.created_at,'Z') DESC,b.id DESC LIMIT ?`, sessionID, after, cursor.CreatedAt, cursor.CreatedAt, cursor.RootID, limit+1)
	if err != nil {
		return page, err
	}
	var targets []harness.SkillMissingReportTarget
	for rows.Next() {
		target := harness.SkillMissingReportTarget{RootSessionID: sessionID}
		if err = rows.Scan(&target.RootID, &target.RootRunID, &target.CreatedAt, &target.Status); err != nil {
			break
		}
		targets = append(targets, target)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	for _, target := range targets {
		if !missingTargetTime(target.CreatedAt) || !missingTargetStatus(target.Status) {
			return page, invalid("missing report target metadata is inconsistent")
		}
		root, err := missingRoot(ctx, tx, sessionID, target.RootRunID)
		if err != nil {
			return page, err
		}
		if root != target.RootID {
			return page, invalid("missing report target root is inconsistent")
		}
		target.ReportRevision, err = missingRevision(ctx, tx, root, sessionID)
		if err != nil {
			return page, err
		}
		if len(page.Targets) == limit {
			page.Next = encodeMissingTargetCursor(cursor)
			break
		}
		page.Targets = append(page.Targets, target)
		cursor.CreatedAt, cursor.RootID = target.CreatedAt, root
	}
	return page, tx.Commit()
}
