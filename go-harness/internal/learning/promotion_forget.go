package learning

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type promotionGarbage struct{ source, name, version string }

// Only exclusively learning-created, unpublished versions may be removed.
// Installed/adopted or independently retained versions and accounting survive.
func forgetPromotionsTx(ctx context.Context, tx *sql.Tx, pattern string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_generation_inputs')`).Scan(&exists); err != nil || !exists {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT dep.job_id,i.source_key,i.name,i.candidate_version,i.proposal_id,
 COALESCE((SELECT status FROM df_skill_proposals WHERE id=i.proposal_id),'')
 FROM df_learning_promotion_dependencies dep LEFT JOIN df_skill_generation_inputs i ON i.job_id=dep.job_id
 WHERE dep.pattern_id=? AND dep.revoked=0`, pattern)
	if err != nil {
		return err
	}
	type affected struct{ job, source, name, version, proposal, status sql.NullString }
	var items []affected
	for rows.Next() {
		var item affected
		if err = rows.Scan(&item.job, &item.source, &item.name, &item.version, &item.proposal, &item.status); err != nil {
			break
		}
		items = append(items, item)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, item := range items {
		if _, err = tx.ExecContext(ctx, `UPDATE df_learning_promotion_dependencies SET revoked=1 WHERE job_id=?`, item.job.String); err != nil {
			return err
		}
		if item.status.String == "published" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM df_learning_owned_versions WHERE source_key=? AND name=? AND version=?`, item.source.String, item.name.String, item.version.String); err != nil {
				return err
			}
			continue
		}
		var adopted bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_publications WHERE ?<>'' AND json_extract(CAST(payload AS TEXT),'$.source_proposal_id')=?)`, item.proposal.String, item.proposal.String).Scan(&adopted); err != nil {
			return err
		}
		if adopted {
			if _, err = tx.ExecContext(ctx, `DELETE FROM df_learning_owned_versions WHERE source_key=? AND name=? AND version=?`, item.source.String, item.name.String, item.version.String); err != nil {
				return err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE state IN ('queued','retry','running') AND (id=? OR id IN (SELECT job_id FROM df_skill_behavior_evaluations WHERE proposal_id=?))`, item.job.String, item.proposal.String); err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM df_skill_behavior_results WHERE evaluation_id IN (SELECT id FROM df_skill_behavior_evaluations WHERE proposal_id=?)`,
			`DELETE FROM df_skill_behavior_evaluations WHERE proposal_id=?`,
			`DELETE FROM df_skill_auto_patch_evidence WHERE proposal_id=?`,
			`DELETE FROM df_skill_auto_patch_observations WHERE proposal_id=?`,
			`DELETE FROM df_skill_proposals WHERE id=?`,
		} {
			if _, err = tx.ExecContext(ctx, query, item.proposal.String); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_generation_inputs WHERE job_id=?`, item.job.String); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_discovery_consumed WHERE discovery_id IN (SELECT id FROM df_skill_discoveries WHERE job_id=?)`, item.job.String); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_discoveries WHERE job_id=?`, item.job.String); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_generation_catalog SET revision=revision+1 WHERE id=1`); err != nil {
			return err
		}
		if item.version.String != "" {
			if err = collectPromotionVersionTx(ctx, tx, promotionGarbage{item.source.String, item.name.String, item.version.String}); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectPromotionVersionTx(ctx context.Context, tx *sql.Tx, v promotionGarbage) error {
	var owned, retained bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_owned_versions WHERE source_key=? AND name=? AND version=?)`, v.source, v.name, v.version).Scan(&owned); err != nil || !owned {
		return err
	}
	queries := []string{
		`SELECT EXISTS(SELECT 1 FROM df_skill_installs WHERE source_key=? AND name=? AND current_version=?)`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_overlays WHERE source_key=? AND name=? AND base_version=?)`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_control WHERE source_key=? AND name=? AND last_import_version=?)`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_publications WHERE source_key=? AND name=? AND
 (json_extract(CAST(payload AS TEXT),'$.sha256')=? OR json_extract(CAST(payload AS TEXT),'$.previous.version')=?))`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_proposals WHERE source_key=? AND name=? AND
 (json_extract(CAST(payload AS TEXT),'$.candidate_sha256')=? OR json_extract(CAST(payload AS TEXT),'$.base.version')=?))`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_generation_inputs WHERE source_key=? AND name=? AND
 (candidate_version=? OR json_extract(CAST(payload AS TEXT),'$.request.base.version')=?))`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_behavior_evaluations WHERE source_key=? AND name=? AND
 (json_extract(CAST(payload AS TEXT),'$.candidate_version')=? OR json_extract(CAST(payload AS TEXT),'$.base.version')=?))`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_exports WHERE source_key=? AND name=? AND json_extract(head,'$.version')=?)`,
		`SELECT EXISTS(SELECT 1 FROM df_skill_management_requests WHERE source_key=? AND name=? AND json_extract(applied_head,'$.version')=?)`,
	}
	for i, query := range queries {
		args := []any{v.source, v.name, v.version}
		if i >= 3 && i <= 6 {
			args = append(args, v.version)
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&retained); err != nil || retained {
			return err
		}
	}
	var reads bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_skill_reads')`).Scan(&reads); err != nil {
		return err
	}
	if reads {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_skill_reads WHERE source_key=? AND name=? AND version=?)`, v.source, v.name, v.version).Scan(&retained); err != nil || retained {
			return err
		}
	}
	for _, query := range []string{
		`DELETE FROM df_skill_files WHERE source_key=? AND name=? AND version=?`,
		`DELETE FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`,
		`DELETE FROM df_learning_owned_versions WHERE source_key=? AND name=? AND version=?`,
	} {
		if _, err := tx.ExecContext(ctx, query, v.source, v.name, v.version); err != nil {
			return err
		}
	}
	return nil
}

func RedactPromotionReason(reason string) (string, error) {
	result := redact(reason, 4000)
	if !validText(result, 4000, true) {
		return "", harness.ErrInvalidInput
	}
	return result, nil
}
