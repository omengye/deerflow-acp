package learning

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func saveConsolidationSourcesTx(ctx context.Context, tx *sql.Tx, job string, snap consolidationSnapshot, choices []patternSnapshot) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_sources VALUES(?,?,?)`, job, snap.Seed.Session, snap.Seed.Epoch); err != nil {
		return err
	}
	for _, choice := range choices {
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_patterns VALUES(?,?)`, job, choice.Pattern.ID); err != nil {
			return err
		}
		if err := walkPatternMembers(ctx, tx, choice, func(dep experienceDependency) error {
			var epoch int64
			err := tx.QueryRowContext(ctx, `SELECT epoch FROM df_learning_consolidation_sources WHERE job_id=? AND session_id=?`, job, dep.Session).Scan(&epoch)
			if err == nil {
				if epoch != dep.Epoch {
					return harness.ErrPermissionDenied
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_sources VALUES(?,?,?)`, job, dep.Session, dep.Epoch)
			return err
		}); err != nil {
			return err
		}
	}
	if snap.Shared {
		_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_sharing VALUES(?,?)`, job, snap.Owner)
		return err
	}
	return nil
}

func verifyConsolidationSourcesTx(ctx context.Context, tx *sql.Tx, job string, snap consolidationSnapshot) error {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_key FROM df_learning_consolidation_sharing WHERE job_id=?`, job).Scan(&owner); err != nil {
		return err
	}
	if owner != snap.Owner {
		return harness.ErrPermissionDenied
	}
	patterns := make([]string, 0, len(snap.Patterns))
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS df_learning_expected_sources(session TEXT,epoch INTEGER,PRIMARY KEY(session,epoch)); DELETE FROM df_learning_expected_sources;`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_expected_sources VALUES(?,?)`, snap.Seed.Session, snap.Seed.Epoch); err != nil {
		return err
	}
	for _, dep := range snap.Patterns {
		patterns = append(patterns, dep.ID)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_learning_expected_sources SELECT e.session_id,m.epoch FROM df_learning_pattern_members m JOIN df_learning_experiences e ON e.id=m.experience_id WHERE m.pattern_id=? AND m.revision<=?`, dep.ID, dep.Revision); err != nil {
			return err
		}
	}
	var mismatch bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT session,epoch FROM df_learning_expected_sources EXCEPT SELECT session_id,epoch FROM df_learning_consolidation_sources WHERE job_id=?) OR EXISTS(SELECT session_id,epoch FROM df_learning_consolidation_sources WHERE job_id=? EXCEPT SELECT session,epoch FROM df_learning_expected_sources)`, job, job).Scan(&mismatch); err != nil {
		return err
	}
	if mismatch {
		return harness.ErrPermissionDenied
	}
	rows, err := tx.QueryContext(ctx, `SELECT pattern_id FROM df_learning_consolidation_patterns WHERE job_id=? ORDER BY pattern_id LIMIT 9`, job)
	if err != nil {
		return err
	}
	var saved []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		saved = append(saved, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	sort.Strings(patterns)
	if !slices.Equal(saved, patterns) {
		return harness.ErrPermissionDenied
	}
	return nil
}

// Edges include the whole shortlist, not just the selected target: negative
// comparisons and saved explanations also carry revocable source material.
func forgetDerivedTx(ctx context.Context, tx *sql.Tx, session string, jobs bool) error {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE affected(kind,id) AS (
 SELECT 'c',c.id FROM df_learning_consolidations c WHERE c.session_id=? OR EXISTS(
 SELECT 1 FROM df_learning_consolidation_sources src WHERE src.job_id=c.job_id AND src.session_id=?)
 UNION SELECT 'p',p.id FROM df_learning_patterns p WHERE p.session_id=? OR EXISTS(
 SELECT 1 FROM df_learning_pattern_members m JOIN df_learning_experiences e ON e.id=m.experience_id WHERE m.pattern_id=p.id AND e.session_id=?)
 UNION SELECT 'p',a.pattern_id FROM df_learning_adjudications a WHERE a.session_id=?
 UNION SELECT 'p',retry.pattern_id FROM df_learning_new_skill_retries retry WHERE retry.session_id=?
 UNION SELECT 'p',r.pattern_id FROM df_learning_pattern_revisions r JOIN affected a ON a.kind='c' AND a.id=r.consolidation_id
 UNION SELECT 'c',c.id FROM df_learning_consolidations c JOIN df_learning_consolidation_patterns dep ON dep.job_id=c.job_id
 JOIN affected a ON a.kind='p' AND a.id=dep.pattern_id)
 SELECT kind,id FROM affected`, session, session, session, session, session, session)
	if err != nil {
		return err
	}
	var consolidations, patterns []string
	for rows.Next() {
		var kind, id string
		if err = rows.Scan(&kind, &id); err != nil {
			break
		}
		if kind == "c" {
			consolidations = append(consolidations, id)
		} else {
			patterns = append(patterns, id)
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range patterns {
		if err = forgetPromotionsTx(ctx, tx, id); err != nil {
			return err
		}
		if jobs {
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE state IN ('queued','retry','running') AND id IN (SELECT job_id FROM df_learning_matches WHERE pattern_id=?)`, id); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_learning_patterns WHERE id=?`, id); err != nil {
			return err
		}
	}
	for _, id := range consolidations {
		if jobs {
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE state IN ('queued','retry','running') AND id IN (SELECT job_id FROM df_learning_consolidations WHERE id=?)`, id); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidation_inputs SET state='cancelled',reason='source_revoked' WHERE job_id IN
 (SELECT job_id FROM df_learning_consolidations WHERE id=?)`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_learning_consolidations WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}
