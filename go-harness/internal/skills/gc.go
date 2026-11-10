package skills

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"time"
)

type ContentMaintenance struct {
	Candidates int
	Bytes      int64
	Reclaimed  int
}

// Only terminal, never-published candidates are reclaimable. Published history
// remains intact, including versions referenced by pre-migration checkpoints.
// The same predicate is rechecked inside the deleting transaction.
const reclaimableCandidate = `EXISTS(SELECT 1 FROM df_skill_proposals p WHERE p.source_key=v.source_key AND p.name=v.name
 AND p.status IN ('rejected','superseded','failed') AND json_extract(CAST(p.payload AS TEXT),'$.candidate_sha256')=v.version)
 AND NOT EXISTS(SELECT 1 FROM df_skill_installs i WHERE i.source_key=v.source_key AND i.name=v.name AND i.current_version=v.version)
 AND NOT EXISTS(SELECT 1 FROM df_skill_control c WHERE c.source_key=v.source_key AND c.name=v.name AND c.last_import_version=v.version)
 AND NOT EXISTS(SELECT 1 FROM df_skill_overlays o WHERE o.source_key=v.source_key AND o.name=v.name AND o.base_version=v.version)
 AND NOT EXISTS(SELECT 1 FROM df_skill_version_references r WHERE r.source_key=v.source_key AND r.name=v.name AND r.version=v.version)
 AND NOT EXISTS(SELECT 1 FROM df_skill_publications p WHERE p.source_key=v.source_key AND p.name=v.name AND instr(CAST(p.payload AS TEXT),v.version)>0)
 AND NOT EXISTS(SELECT 1 FROM df_skill_proposals p WHERE p.source_key=v.source_key AND p.name=v.name AND p.status NOT IN ('rejected','superseded','failed') AND instr(CAST(p.payload AS TEXT),v.version)>0)
 AND NOT EXISTS(SELECT 1 FROM df_skill_file_publications p WHERE p.source_key=v.source_key AND p.name=v.name)
 AND NOT EXISTS(SELECT 1 FROM df_skill_edits e WHERE e.source_key=v.source_key AND e.name=v.name AND e.status IN ('applying','publishing','needs_reconciliation'))
 AND NOT EXISTS(SELECT 1 FROM df_skill_exports e WHERE e.source_key=v.source_key AND e.name=v.name AND instr(CAST(e.head AS TEXT),v.version)>0)
 AND NOT EXISTS(SELECT 1 FROM df_skill_behavior_evaluations e JOIN df_skill_maintenance_jobs j ON j.id=e.job_id WHERE e.source_key=v.source_key AND e.name=v.name AND j.state IN ('queued','retry','running','waiting') AND instr(CAST(e.payload AS TEXT),v.version)>0)
 AND NOT EXISTS(SELECT 1 FROM df_skill_content_gc g WHERE g.source_key=v.source_key AND g.name=v.name AND g.version=v.version AND g.state='expired')`

func (r *Registry) MaintainCandidates(ctx context.Context, limit int, apply bool) (ContentMaintenance, error) {
	var report ContentMaintenance
	if limit < 1 || limit > 128 {
		return report, invalid("content maintenance batch exceeds bounds")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return report, ErrClosed
	}
	var available int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('df_skill_proposals','df_skill_publications','df_skill_behavior_evaluations','df_skill_maintenance_jobs','df_skill_generation_outputs','df_skill_generation_inputs')`).Scan(&available); err != nil {
		return report, err
	}
	if available != 6 {
		return report, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if apply {
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_catalog SET revision=revision WHERE id=1`); err != nil {
			return report, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_key,name,version,total_bytes FROM df_skill_versions v WHERE `+reclaimableCandidate+` ORDER BY created_at,source_key,name,version LIMIT ?`, limit)
	if err != nil {
		return report, err
	}
	type version struct {
		source, name, sha string
		size              int64
	}
	var versions []version
	for rows.Next() {
		var v version
		if err = rows.Scan(&v.source, &v.name, &v.sha, &v.size); err != nil {
			break
		}
		versions = append(versions, v)
		report.Candidates++
		report.Bytes += v.size
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return report, err
	}
	if !apply {
		return report, nil
	}
	for _, v := range versions {
		// Marking and rechecking are durable even if physical object cleanup is
		// interrupted. Pin acquisition cannot race this transaction.
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_content_gc VALUES(?,?,?,'deleting',?) ON CONFLICT(source_key,name,version) DO UPDATE SET state='deleting'`, v.source, v.name, v.sha, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_object_gc SELECT hash,'pending' FROM df_skill_files WHERE source_key=? AND name=? AND version=?`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_file_objects WHERE source_key=? AND name=? AND version=?`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_files WHERE source_key=? AND name=? AND version=?`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_content_gc SET state='expired' WHERE source_key=? AND name=? AND version=?`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_proposals SET payload=json_set(CAST(payload AS TEXT),'$.content_expired',json('true'),'$.diff','') WHERE source_key=? AND name=? AND status IN ('rejected','superseded','failed') AND json_extract(CAST(payload AS TEXT),'$.candidate_sha256')=?`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_generation_outputs WHERE job_id IN (SELECT g.job_id FROM df_skill_generation_inputs g JOIN df_skill_proposals p ON p.id=g.proposal_id WHERE g.source_key=? AND g.name=? AND g.candidate_version=? AND p.status IN ('rejected','superseded','failed'))`, v.source, v.name, v.sha); err != nil {
			return report, err
		}
		report.Reclaimed++
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	return report, r.cleanUnreferencedObjects(ctx, limit)
}

func (r *Registry) cleanUnreferencedObjects(ctx context.Context, limit int) error {
	for i := 0; i < limit; i++ {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var sha string
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_catalog SET revision=revision WHERE id=1`); err != nil {
			tx.Rollback()
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT hash FROM df_skill_object_gc WHERE state='pending' ORDER BY hash LIMIT 1`).Scan(&sha)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			return nil
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		var referenced bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_files WHERE hash=?)`, sha).Scan(&referenced); err != nil {
			tx.Rollback()
			return err
		}
		if !referenced {
			if !validHash(sha) {
				tx.Rollback()
				return invalid("invalid content object identity")
			}
			if err = r.objects.Remove(objectName(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
				tx.Rollback()
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_object_gc WHERE hash=?`, sha); err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
