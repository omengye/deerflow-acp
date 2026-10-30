package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Terminal uncertainty is historical evidence, not ongoing execution. Archive
// preserves it; explicit deletion may remove it together with its session.
// Pending/started calls remain blockers even if a run row says it has ended.
const sessionCleanupReceiptBlockerSQL = `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts r WHERE r.session_id=? AND (r.state IN ('pending','started') OR (r.state='uncertain' AND NOT EXISTS(SELECT 1 FROM harness_runs ended WHERE ended.id=r.run_id AND ended.session_id=r.session_id AND ended.status IN ('completed','failed','cancelled')))))`

// ArchiveExpiredSession retires only derived resident state. The coordinator
// fence excludes reconnects; the writer transaction makes the age, execution,
// receipt and background graph checks consistent with the archive marker.
func (s *Service) ArchiveExpiredSession(ctx context.Context, id string, now time.Time, policy harness.RetentionPolicy) (bool, error) {
	if !policy.Enabled {
		return false, nil
	}
	if err := ValidateRetention(policy); err != nil {
		return false, err
	}
	release, err := s.Coordinator.ReserveCleanup(id)
	if err != nil {
		return false, err
	}
	defer release()
	archived, assetIDs, err := s.Store.archiveSession(ctx, id, now, policy)
	if err != nil || !archived {
		return archived, err
	}
	// This cache is process-local: a crash before eviction also releases it.
	// Eviction cannot fail and never touches original artifacts or attachments.
	if s.Assets != nil {
		s.Assets.EvictArtifactReadCache(assetIDs)
	}
	return true, nil
}

func (s *Store) archiveSession(ctx context.Context, id string, now time.Time, policy harness.RetentionPolicy) (bool, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=updated_at WHERE id=?`, id); err != nil {
		return false, nil, err
	}
	var closed, updated, archived string
	err = tx.QueryRowContext(ctx, `SELECT closed_at,updated_at,archived_at FROM harness_sessions WHERE id=?`, id).Scan(&closed, &updated, &archived)
	if errors.Is(err, sql.ErrNoRows) || archived != "" {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	raw, days := updated, policy.InactiveDays
	if closed != "" {
		raw, days = closed, policy.ClosedDays
	}
	when, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || when.After(now.UTC().AddDate(0, 0, -days)) {
		return false, nil, err
	}
	tables, err := sessionTables(ctx, tx)
	if err != nil {
		return false, nil, err
	}
	children, _, err := prepareBackgroundGraphTx(ctx, tx, id, tables, true)
	if err != nil {
		return false, nil, err
	}
	var assetIDs []string
	for _, target := range append(children, id) {
		if err = checkArchiveExecutionTx(ctx, tx, target, tables); err != nil {
			return false, nil, err
		}
		var unresolved bool
		if err = tx.QueryRowContext(ctx, sessionCleanupReceiptBlockerSQL, target).Scan(&unresolved); err != nil {
			return false, nil, err
		}
		if unresolved {
			return false, nil, harness.ErrReceiptConflict
		}
		if tables["harness_assets"] {
			rows, queryErr := tx.QueryContext(ctx, `SELECT id FROM harness_assets WHERE session_id=?`, target)
			if queryErr != nil {
				return false, nil, queryErr
			}
			for rows.Next() {
				var assetID string
				if err = rows.Scan(&assetID); err != nil {
					break
				}
				assetIDs = append(assetIDs, assetID)
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil {
				return false, nil, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET archived_at=? WHERE id=?`, now.UTC().Format(time.RFC3339Nano), target); err != nil {
			return false, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, assetIDs, nil
}

func checkArchiveExecutionTx(ctx context.Context, tx *sql.Tx, id string, tables map[string]bool) error {
	// Explicit terminal allowlists also protect future/unknown execution states.
	for _, check := range []struct{ table, query string }{
		{"harness_runs", `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status NOT IN ('completed','failed','cancelled'))`},
		{"harness_executions", `SELECT EXISTS(SELECT 1 FROM harness_executions WHERE session_id=? AND status NOT IN ('completed','failed','cancelled'))`},
		{"harness_execution_attempts", `SELECT EXISTS(SELECT 1 FROM harness_execution_attempts a JOIN harness_runs r ON r.id=a.run_id WHERE r.session_id=? AND a.status='running')`},
		{"budget_attempts", `SELECT EXISTS(SELECT 1 FROM budget_attempts a JOIN budget_roots r ON r.id=a.root_id WHERE r.session_id=? AND a.state IN ('active','unknown'))`},
		{"budget_reservations", `SELECT EXISTS(SELECT 1 FROM budget_reservations v JOIN budget_roots r ON r.id=v.root_id WHERE r.session_id=? AND v.state IN ('reserved','dispatched','unknown'))`},
	} {
		if !tables[check.table] {
			continue
		}
		var blocked bool
		if err := tx.QueryRowContext(ctx, check.query, id).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return harness.ErrBusy
		}
	}
	return nil
}
