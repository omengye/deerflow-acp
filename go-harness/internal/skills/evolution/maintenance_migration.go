package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func bindLegacyMaintenance(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_maintenance_jobs WHERE job_kind='legacy' ORDER BY rowid`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		job, err := skillmaintenance.ReadJob(ctx, tx, id)
		if err != nil {
			return err
		}
		var generation, behavior bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_generation_inputs WHERE job_id=?),EXISTS(SELECT 1 FROM df_skill_behavior_evaluations WHERE job_id=? AND manual=1)`, id, id).Scan(&generation, &behavior); err != nil {
			return err
		}
		if generation == behavior {
			continue // Unknown or ambiguous producers remain fail-closed.
		}
		kind := skillmaintenance.SkillGeneration
		if generation {
			if _, err = readGeneration(ctx, tx, id); err != nil {
				return err
			}
		} else {
			proposalID, err := manualBehaviorProposal(ctx, tx, id)
			if err != nil {
				return err
			}
			r, err := readBehavior(ctx, tx, proposalID)
			if err != nil {
				return err
			}
			p, err := readProposal(ctx, tx, proposalID)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(r.input)
			if err != nil {
				return err
			}
			if !r.manual || r.jobID != id || r.input.ID != job.Request.Reference || hash(payload) != job.Request.InputDigest || job.Request.SourceIdentity != p.Target.SourceIdentity || job.Request.Name != p.Target.Name || p.BehaviorJobID != id {
				return harness.ErrSkillConflict
			}
			kind = skillmaintenance.SkillBehavior
		}
		if err = skillmaintenance.BindLegacyKindTx(ctx, tx, job, kind); err != nil {
			return err
		}
	}
	return nil
}
