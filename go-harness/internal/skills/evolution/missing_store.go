package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxMissingReportRevisions = 1000

func migrateMissingReports(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_missing_catalog(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL);
INSERT OR IGNORE INTO df_skill_missing_catalog VALUES(1,0);
CREATE TABLE IF NOT EXISTS df_skill_missing_reports (
 root_id TEXT PRIMARY KEY REFERENCES budget_roots(id) ON DELETE CASCADE,
 id TEXT UNIQUE NOT NULL,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS df_skill_missing_session ON df_skill_missing_reports(session_id,root_id);
CREATE TABLE IF NOT EXISTS df_skill_missing_revisions (
 root_id TEXT NOT NULL REFERENCES df_skill_missing_reports(root_id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,run_id TEXT NOT NULL,request_key TEXT NOT NULL,request_sha TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,
 PRIMARY KEY(root_id,revision),UNIQUE(root_id,request_key));
CREATE TRIGGER IF NOT EXISTS df_skill_missing_created AFTER INSERT ON df_skill_missing_revisions
 BEGIN UPDATE df_skill_missing_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_missing_deleted AFTER DELETE ON df_skill_missing_revisions
 BEGIN UPDATE df_skill_missing_catalog SET revision=revision+1 WHERE id=1; END;`)
	// RunID is deliberately checked against the live task graph instead of a
	// restrictive FK: root deletion purges private child runs before budget roots.
	return err
}

func validMissingIdentity(value string) bool {
	if value == "" || !bounded(value, 256) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return false
		}
	}
	return true
}

func missingKind(kind string) bool { return kind == "missing_skill" || kind == "withdrawn" }

func missingReportID(root string) string {
	return "missing_" + encodedHash([]string{"missing-skill-v1", root})
}

// Match the foreground restriction of runtime.ManagementSession, but query on
// this transaction: the shared SQLite store may have only one connection.
// Local operator APIs do not require a live ACP connection owner.
func missingSession(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, sessionID).Scan(&present); err != nil {
		return err
	}
	if !present {
		return harness.ErrNotFound
	}
	for _, table := range []string{"harness_background_specs", "harness_background_bindings"} {
		var hasBackground bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&hasBackground); err != nil {
			return err
		}
		if !hasBackground {
			continue
		}
		var private bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE child_session_id=?)`, sessionID).Scan(&private); err != nil {
			return err
		}
		if private {
			return harness.ErrNotFound
		}
	}
	return nil
}

// Resolve both the requested run and the original root run through real run
// rows. Budget member/session columns alone are not a runtime ownership proof.
func missingRoot(ctx context.Context, tx *sql.Tx, sessionID, runID string) (string, error) {
	if err := missingSession(ctx, tx, sessionID); err != nil {
		return "", err
	}
	var rootID, rootSession, rootRun, rootOrigin, rootParent, rootKind string
	err := tx.QueryRowContext(ctx, `SELECT b.id,b.session_id,b.root_run_id,rm.origin_id,rm.parent_id,rm.kind
 FROM harness_runs r JOIN budget_members m ON m.id=r.id AND m.session_id=r.session_id
 JOIN budget_roots b ON b.id=m.root_id
 JOIN harness_runs rr ON rr.id=b.root_run_id AND rr.session_id=b.session_id
 JOIN budget_members rm ON rm.id=rr.id AND rm.root_id=b.id AND rm.session_id=rr.session_id
 WHERE r.id=?`, runID).Scan(&rootID, &rootSession, &rootRun, &rootOrigin, &rootParent, &rootKind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", harness.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if rootSession != sessionID {
		return "", harness.ErrPermissionDenied
	}
	if !validMissingIdentity(rootID) || !validMissingIdentity(rootRun) || rootOrigin != rootRun || rootParent != "" || rootKind != "run" {
		return "", invalid("missing report root binding is inconsistent")
	}
	return rootID, nil
}

func missingRevision(ctx context.Context, tx *sql.Tx, rootID, sessionID string) (int64, error) {
	var savedID, savedSession string
	err := tx.QueryRowContext(ctx, `SELECT id,session_id FROM df_skill_missing_reports WHERE root_id=?`, rootID).Scan(&savedID, &savedSession)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if savedID != missingReportID(rootID) || savedSession != sessionID {
		return 0, invalid("missing report identity is inconsistent")
	}
	var revision, count int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0),count(*) FROM df_skill_missing_revisions WHERE root_id=?`, rootID).Scan(&revision, &count)
	if err == nil && (revision < 1 || revision > maxMissingReportRevisions || count != revision) {
		err = invalid("missing report history is inconsistent")
	}
	return revision, err
}

func readMissingReport(ctx context.Context, tx *sql.Tx, rootID, sessionID string, revision int64) (harness.SkillMissingReport, error) {
	var report harness.SkillMissingReport
	var payload []byte
	var digest, requestSHA, runID string
	err := tx.QueryRowContext(ctx, `SELECT run_id,payload,payload_sha,request_sha FROM df_skill_missing_revisions WHERE root_id=? AND revision=?`, rootID, revision).Scan(&runID, &payload, &digest, &requestSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return report, harness.ErrNotFound
	}
	if err != nil {
		return report, err
	}
	if len(payload) > 32<<10 || hash(payload) != digest || !hexDigest(requestSHA) || json.Unmarshal(payload, &report) != nil ||
		report.ID != missingReportID(rootID) || report.RootID != rootID || report.RootSessionID != sessionID || report.RunID != runID || !validMissingIdentity(runID) ||
		report.Revision != revision || revision < 1 || revision > maxMissingReportRevisions || report.Origin != "operator" || report.Attribution != "unassessed" || !missingKind(report.Kind) {
		return harness.SkillMissingReport{}, invalid("missing report record is inconsistent")
	}
	root, err := missingRoot(ctx, tx, sessionID, runID)
	if err != nil {
		return harness.SkillMissingReport{}, err
	}
	if root != rootID {
		return harness.SkillMissingReport{}, invalid("missing report run was rebound")
	}
	summary, _, err := cleanFeedbackSummary(report.Summary)
	if err != nil || summary != report.Summary || (report.Kind == "withdrawn") != (summary == "") {
		return harness.SkillMissingReport{}, invalid("missing report summary is inconsistent")
	}
	if _, err := time.Parse(time.RFC3339Nano, report.CreatedAt); err != nil {
		return harness.SkillMissingReport{}, invalid("missing report timestamp is inconsistent")
	}
	return report, nil
}
