package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const (
	matchReassessmentCooldown = time.Minute
	matchReassessmentWindow   = 30 * 24 * time.Hour
)

// Fresh assessment is allowed only before any generation reservation and on a
// real catalog change. Never resurrect cancelled work or refund paid attempts.
func (s *Store) queueMatchReassessmentTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var sequence int64
	var jobID, created, firstCreated string
	err := tx.QueryRowContext(ctx, `SELECT i.sequence,i.job_id,m.created_at,
 (SELECT first.created_at FROM df_learning_matches first WHERE first.pattern_id=p.id ORDER BY first.rowid LIMIT 1)
 FROM df_learning_match_inputs i JOIN df_learning_patterns p ON p.id=i.pattern_id AND p.current_revision=i.revision
 JOIN df_learning_matches m ON m.job_id=i.job_id AND m.pattern_id=i.pattern_id AND m.revision=i.revision
 JOIN df_skill_maintenance_jobs j ON j.id=i.job_id
 WHERE i.sequence>(SELECT cursor FROM df_learning_match_scan WHERE id=1)
 AND (i.state='processed' AND (j.state='completed' OR j.state='failed' AND j.error_code='inventory_changed')
 OR i.state='skipped' AND j.state='failed' AND j.error_code='inventory_changed')
 AND CASE WHEN json_valid(CAST(m.payload AS TEXT)) THEN json_extract(CAST(m.payload AS TEXT),'$.catalog_revision') END<>(SELECT revision FROM df_skill_catalog WHERE id=1)
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_reviews review WHERE review.pattern_id=p.id AND review.state_version>0)
 AND NOT EXISTS(SELECT 1 FROM df_learning_promotion_dependencies dep WHERE dep.pattern_id=p.id)
 AND (SELECT count(*) FROM df_learning_match_reassessments a WHERE a.pattern_id=p.id)<2
 AND NOT EXISTS(SELECT 1 FROM df_learning_matches old JOIN df_skill_maintenance_jobs active ON active.id=old.job_id
 WHERE old.pattern_id=p.id AND active.state IN ('queued','retry','running','cancelled'))
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_members o WHERE o.pattern_id=p.id AND o.revision=i.revision AND `+episodeTailActivePredicate+`)
 ORDER BY i.sequence LIMIT 1`).Scan(&sequence, &jobID, &created, &firstCreated)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `UPDATE df_learning_match_scan SET cursor=0 WHERE id=1`)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_match_scan SET cursor=? WHERE id=1`, sequence); err != nil {
		return false, err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	now := s.now().UTC()
	firstAt, firstErr := time.Parse(time.RFC3339Nano, firstCreated)
	if err != nil || firstErr != nil || now.Before(firstAt) || now.Before(createdAt.Add(matchReassessmentCooldown)) || !now.Before(firstAt.Add(matchReassessmentWindow)) {
		return false, tx.Commit()
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	snap, _, err := s.readMatchRecordTx(ctx, tx, job, true)
	if err != nil {
		return matchReassessmentFailure(ctx, tx, err)
	}
	p, err := s.patternTx(ctx, tx, snap.Pattern.ID, snap.Pattern.Revision, true)
	if err != nil {
		return matchReassessmentFailure(ctx, tx, err)
	}
	if p.Pattern.SessionID != snap.Session || encodedDigest(p) != snap.Pattern.SHA {
		return false, tx.Commit()
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner_key FROM df_learning_patterns WHERE id=?`, p.Pattern.ID).Scan(&owner); err != nil {
		return false, err
	}
	if owner != snap.Owner {
		return false, tx.Commit()
	}
	switch p.Pattern.EvidenceState {
	case "conflicting", "ambiguous", "unresolved":
		return false, tx.Commit()
	}
	if err = s.guardPatternConsolidationTx(ctx, tx, p); err != nil {
		return matchReassessmentFailure(ctx, tx, err)
	}
	if err = s.GuardNewPromotionTx(ctx, tx, p.Pattern.ID, ""); err != nil {
		return matchReassessmentFailure(ctx, tx, err)
	}
	inventory, choices, view, err := s.matchViewTx(ctx, tx, p)
	if err != nil {
		return matchReassessmentFailure(ctx, tx, err)
	}
	data, err := json.Marshal(view)
	if err != nil || len(data) > s.matchInputLimit() || inventory.Catalog == snap.Catalog {
		return false, tx.Commit()
	}
	return s.enqueueMatchTx(ctx, tx, sequence, snap.Session, snap.Owner, p, inventory, choices, view, &snap)
}

func matchReassessmentFailure(ctx context.Context, tx *sql.Tx, err error) (bool, error) {
	if errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	return false, err
}
