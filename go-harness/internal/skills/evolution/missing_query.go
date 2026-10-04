package evolution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func missingCursor(after, kind string, filter []string) (pageCursor, error) {
	cursor := pageCursor{Kind: kind, Filter: encodedHash(filter)}
	if after == "" {
		return cursor, nil
	}
	var saved pageCursor
	data, err := base64.RawURLEncoding.Strict().DecodeString(after)
	// Cursors are opaque; accepting only our canonical encoding also rejects
	// unknown fields, duplicate keys, trailing JSON and whitespace aliases.
	if err != nil || json.Unmarshal(data, &saved) != nil || encodeCursor(saved) != after || saved.Kind != kind || saved.Filter != cursor.Filter || !validMissingIdentity(saved.Key) || saved.Name != "" || saved.Revision < 0 {
		return cursor, invalid("missing report cursor does not match query")
	}
	return saved, nil
}

// QueryMissingReports returns one current judgment per root, including explicit
// withdrawals. Revisions never produce additional root votes.
func (s *Service) QueryMissingReports(ctx context.Context, sessionID, after string, limit int) (harness.SkillMissingReportPage, error) {
	page := harness.SkillMissingReportPage{Reports: []harness.SkillMissingReport{}}
	if !validMissingIdentity(sessionID) || len(after) > 2048 || limit < 1 || limit > 100 {
		return page, invalid("missing report query is invalid")
	}
	cursor, err := missingCursor(after, "skill-missing-list-v1", []string{sessionID})
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
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_missing_catalog WHERE id=1`).Scan(&page.CatalogVersion); err != nil {
		return page, err
	}
	if after != "" && cursor.Revision != page.CatalogVersion {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.CatalogVersion
	rows, err := tx.QueryContext(ctx, `SELECT root_id FROM df_skill_missing_reports WHERE session_id=? AND root_id>? ORDER BY root_id LIMIT ?`, sessionID, cursor.Key, limit+1)
	if err != nil {
		return page, err
	}
	var roots []string
	for rows.Next() {
		var root string
		if err = rows.Scan(&root); err != nil {
			break
		}
		roots = append(roots, root)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	bytesUsed := 0
	for _, root := range roots {
		if len(page.Reports) == limit {
			page.Next = encodeCursor(cursor)
			break
		}
		revision, err := missingRevision(ctx, tx, root, sessionID)
		if err != nil {
			return page, err
		}
		report, err := readMissingReport(ctx, tx, root, sessionID, revision)
		if err != nil {
			return page, err
		}
		data, err := json.Marshal(report)
		if err != nil {
			return page, err
		}
		if bytesUsed+len(data) > 2<<20 && len(page.Reports) > 0 {
			page.Next = encodeCursor(cursor)
			break
		}
		bytesUsed += len(data)
		page.Reports = append(page.Reports, report)
		cursor.Key = root
	}
	return page, tx.Commit()
}

func (s *Service) QueryMissingReportHistory(ctx context.Context, sessionID, runID, after string, limit int) (harness.SkillMissingReportHistoryPage, error) {
	page := harness.SkillMissingReportHistoryPage{Revisions: []harness.SkillMissingReport{}}
	if !validMissingIdentity(sessionID) || !validMissingIdentity(runID) || len(after) > 2048 || limit < 1 || limit > 100 {
		return page, invalid("missing report history query is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	root, err := missingRoot(ctx, tx, sessionID, runID)
	if err != nil {
		return page, err
	}
	cursor, err := missingCursor(after, "skill-missing-history-v1", []string{sessionID, root})
	if err != nil {
		return page, err
	}
	var last int64
	if after != "" {
		last, err = strconv.ParseInt(cursor.Key, 10, 64)
		if err != nil || last < 1 || last >= maxMissingReportRevisions || strconv.FormatInt(last, 10) != cursor.Key {
			return page, invalid("missing report history cursor is invalid")
		}
	}
	page.CurrentRevision, err = missingRevision(ctx, tx, root, sessionID)
	if err != nil {
		return page, err
	}
	if after != "" && cursor.Revision != page.CurrentRevision {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.CurrentRevision
	if after != "" && last >= page.CurrentRevision {
		return page, invalid("missing report history cursor is beyond its snapshot")
	}
	bytesUsed := 0
	for revision := last + 1; revision <= page.CurrentRevision; revision++ {
		if len(page.Revisions) == limit {
			page.Next = encodeCursor(cursor)
			break
		}
		report, err := readMissingReport(ctx, tx, root, sessionID, revision)
		if err != nil {
			return page, err
		}
		data, err := json.Marshal(report)
		if err != nil {
			return page, err
		}
		if bytesUsed+len(data) > 2<<20 && len(page.Revisions) > 0 {
			page.Next = encodeCursor(cursor)
			break
		}
		bytesUsed += len(data)
		page.Revisions = append(page.Revisions, report)
		cursor.Key = strconv.FormatInt(revision, 10)
	}
	return page, tx.Commit()
}
