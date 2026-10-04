package evolution

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func hexDigest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func readSignal(ctx context.Context, tx *sql.Tx, id string, allowed map[string]harness.SkillTarget) (harness.SkillSignal, error) {
	var signal harness.SkillSignal
	var workspace string
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.root_id,s.session_id,s.source_id,s.source_key,s.scope,s.name,s.version,s.state_version,h.cwd
 FROM df_skill_signals s JOIN harness_sessions h ON h.id=s.session_id WHERE s.id=?`, id).Scan(&signal.ID, &signal.RootID, &signal.RootSessionID, &signal.Target.SourceID, &signal.Target.SourceIdentity, &signal.Target.Scope, &signal.Target.Name, &signal.Version, &signal.StateVersion, &workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return signal, harness.ErrNotFound
	}
	if err != nil {
		return signal, err
	}
	configured, exists := allowed[signal.Target.SourceIdentity]
	if !exists || configured.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(workspace, configured.Workspace) {
		return signal, harness.ErrPermissionDenied
	}
	signal.Target.Workspace = configured.Workspace
	if !checkTarget(allowed, signal.Target) {
		return signal, harness.ErrPermissionDenied
	}
	if signal.ID != signalID(signal.RootID, signal.Target.SourceIdentity, signal.Target.Name, signal.Version) || !hexDigest(signal.Version) || signal.StateVersion < 1 {
		return signal, errCollectionEvidence
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT r.run_id),count(DISTINCT NULLIF(r.role_id,'')),
 count(DISTINCT CASE WHEN o.status='completed' THEN r.run_id END),
 count(DISTINCT CASE WHEN o.status='failed' THEN r.run_id END),
 count(DISTINCT CASE WHEN o.status='cancelled' THEN r.run_id END),
 count(DISTINCT CASE WHEN o.effects_uncertain<>0 THEN r.run_id END)
 FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id JOIN harness_skill_outcomes o ON o.sequence=l.outcome_seq WHERE l.signal_id=?`, id).
		Scan(&signal.ReadCount, &signal.RunCount, &signal.RoleCount, &signal.CompletedRuns, &signal.FailedRuns, &signal.CancelledRuns, &signal.UncertainRuns)
	if err != nil {
		return signal, err
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id LEFT JOIN harness_skill_outcomes o ON o.run_id=r.id
 WHERE m.root_id=? AND (r.status NOT IN ('completed','failed','cancelled') OR o.processed=0))`, signal.RootID).Scan(&signal.CollectionPending)
	if err != nil {
		return signal, err
	}
	signal.Attribution = "unassessed"
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM df_skill_feedback WHERE signal_id=?`, signal.ID).Scan(&signal.FeedbackRevision)
	if err != nil {
		return signal, err
	}
	if signal.FeedbackRevision < 0 || signal.FeedbackRevision > maxFeedbackRevisions {
		return signal, errCollectionEvidence
	}
	signal.Quality, err = AssessSignalTx(ctx, tx, signal)
	if err != nil {
		return signal, err
	}
	return signal, nil
}

// QuerySignals is operator-only and limits visibility to currently configured
// source identities. One row per root/version cannot inflate repeated failures
// merely because multiple children or retries loaded that same version.
func (s *Service) QuerySignals(ctx context.Context, q harness.SkillQuery) (harness.SkillSignalPage, error) {
	page := harness.SkillSignalPage{Signals: []harness.SkillSignal{}}
	if q.Status != "" {
		return page, invalid("signals do not accept proposal status")
	}
	allowed, where, args, cursor, err := s.querySources(q, "skill-signals-v1")
	if err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_signal_catalog WHERE id=1`).Scan(&page.CatalogVersion); err != nil {
		return page, err
	}
	if q.Cursor != "" && cursor.Revision != page.CatalogVersion {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.CatalogVersion
	where += " AND id>?"
	args = append(args, cursor.Key, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_signals WHERE `+where+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	for _, id := range ids {
		if len(page.Signals) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		signal, err := readSignal(ctx, tx, id, allowed)
		if err != nil {
			return page, err
		}
		page.Signals = append(page.Signals, signal)
		cursor.Key = id
	}
	return page, tx.Commit()
}

func (s *Service) SignalEvidence(ctx context.Context, id, after string, limit int) (harness.SkillSignalEvidence, error) {
	page := harness.SkillSignalEvidence{Reads: []harness.SkillSignalRead{}}
	if !strings.HasPrefix(id, "sig_") || !hexDigest(strings.TrimPrefix(id, "sig_")) {
		return page, invalid("signal id is invalid")
	}
	allowed, _, _, cursor, err := s.querySources(harness.SkillQuery{Name: id, Cursor: after, Limit: limit}, "skill-signal-reads-v1")
	if err != nil {
		return page, err
	}
	var last int64
	if cursor.Key != "" {
		last, err = strconv.ParseInt(cursor.Key, 10, 64)
		if err != nil || last < 1 {
			return page, invalid("signal evidence cursor is invalid")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page.Signal, err = readSignal(ctx, tx, id, allowed)
	if err != nil {
		return page, err
	}
	if after != "" && cursor.Revision != page.Signal.StateVersion {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.Signal.StateVersion
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.run_id,r.session_id,r.attempt_id,r.role_id,r.kind,r.path,r.file_hash,r.bytes,r.observed_at,o.status,o.effects_uncertain
 FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id JOIN harness_skill_outcomes o ON o.sequence=l.outcome_seq
 WHERE l.signal_id=? AND r.id>? ORDER BY r.id LIMIT ?`, id, last, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		if len(page.Reads) == limit {
			page.Next = encodeCursor(cursor)
			break
		}
		var read harness.SkillSignalRead
		if err = rows.Scan(&read.ID, &read.RunID, &read.SessionID, &read.AttemptID, &read.RoleID, &read.Kind, &read.Path, &read.Hash, &read.Bytes, &read.ObservedAt, &read.Outcome, &read.Uncertain); err != nil {
			break
		}
		page.Reads = append(page.Reads, read)
		cursor.Key = strconv.FormatInt(read.ID, 10)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	return page, tx.Commit()
}

func (s *Service) CollectorStatus(ctx context.Context) (harness.SkillCollectorStatus, error) {
	status := harness.SkillCollectorStatus{Jobs: []harness.SkillCollectionJob{}}
	s.collector.mu.Lock()
	status.Running = s.collector.done != nil && !s.collector.closed
	s.collector.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return status, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM harness_skill_outcomes WHERE processed=0),
 (SELECT count(*) FROM harness_skill_outcomes WHERE processed=1),
 (SELECT count(*) FROM df_skill_collection_jobs WHERE state='failed')`).Scan(&status.Pending, &status.Completed, &status.Failed)
	if err != nil {
		return status, err
	}
	page, err := collectionJobs(ctx, tx, 0, 100)
	if err != nil {
		return status, err
	}
	status.Jobs, status.Truncated = page.Jobs, page.Next != 0
	return status, tx.Commit()
}

// Queue pages are a live keyset inventory. Re-read the first page to refresh
// mutable job states; completed/deleted jobs can disappear between pages.
func (s *Service) QueryCollectionJobs(ctx context.Context, after int64, limit int) (harness.SkillCollectionJobPage, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return harness.SkillCollectionJobPage{}, invalid("collection query is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.SkillCollectionJobPage{}, err
	}
	defer tx.Rollback()
	page, err := collectionJobs(ctx, tx, after, limit)
	if err != nil {
		return page, err
	}
	return page, tx.Commit()
}

func collectionJobs(ctx context.Context, tx *sql.Tx, after int64, limit int) (harness.SkillCollectionJobPage, error) {
	page := harness.SkillCollectionJobPage{Jobs: []harness.SkillCollectionJob{}}
	rows, err := tx.QueryContext(ctx, `SELECT o.sequence,o.session_id,o.run_id,COALESCE(j.state_version,0),COALESCE(j.state,'unclaimed'),COALESCE(j.read_cursor,0),COALESCE(j.attempts,0),COALESCE(j.error_code,''),COALESCE(j.retry_ns,0)
 FROM harness_skill_outcomes o LEFT JOIN df_skill_collection_jobs j ON j.outcome_seq=o.sequence WHERE o.processed=0 AND o.sequence>? ORDER BY o.sequence LIMIT ?`, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		if len(page.Jobs) == limit {
			page.Next = page.Jobs[len(page.Jobs)-1].OutcomeID
			break
		}
		var job harness.SkillCollectionJob
		if err = rows.Scan(&job.OutcomeID, &job.SessionID, &job.RunID, &job.StateVersion, &job.State, &job.ReadCursor, &job.Attempts, &job.ErrorCode, &job.RetryAfterNS); err != nil {
			break
		}
		page.Jobs = append(page.Jobs, job)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return page, err
}
