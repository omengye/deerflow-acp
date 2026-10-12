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
	sources := map[string]int64{snap.Seed.Session: snap.Seed.Epoch}
	for _, choice := range choices {
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_patterns VALUES(?,?)`, job, choice.Pattern.ID); err != nil {
			return err
		}
		for _, dep := range choice.Members {
			if prior, exists := sources[dep.Session]; exists && prior != dep.Epoch {
				return harness.ErrPermissionDenied
			}
			sources[dep.Session] = dep.Epoch
		}
	}
	for session, epoch := range sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_consolidation_sources VALUES(?,?,?)`, job, session, epoch); err != nil {
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
	expected := map[string]int64{snap.Seed.Session: snap.Seed.Epoch}
	patterns := make([]string, 0, len(snap.Patterns))
	for _, dep := range snap.Patterns {
		patterns = append(patterns, dep.ID)
		rows, err := tx.QueryContext(ctx, `SELECT e.session_id,m.epoch FROM df_learning_pattern_members m
 JOIN df_learning_experiences e ON e.id=m.experience_id WHERE m.pattern_id=? AND m.revision=? LIMIT ?`, dep.ID, dep.Revision, maxPatternMembers+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var session string
			var epoch int64
			if err = rows.Scan(&session, &epoch); err != nil {
				break
			}
			if prior, exists := expected[session]; exists && prior != epoch {
				err = harness.ErrPermissionDenied
				break
			}
			expected[session] = epoch
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id,epoch FROM df_learning_consolidation_sources WHERE job_id=? LIMIT ?`, job, 8*maxPatternMembers+2)
	if err != nil {
		return err
	}
	actual := map[string]int64{}
	for rows.Next() {
		var session string
		var epoch int64
		if err = rows.Scan(&session, &epoch); err != nil {
			break
		}
		actual[session] = epoch
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	if encodedDigest(actual) != encodedDigest(expected) {
		return harness.ErrPermissionDenied
	}
	rows, err = tx.QueryContext(ctx, `SELECT pattern_id FROM df_learning_consolidation_patterns WHERE job_id=? ORDER BY pattern_id LIMIT 9`, job)
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
