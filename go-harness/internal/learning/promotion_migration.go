package learning

import (
	"context"
	"database/sql"
	"errors"
)

func migratePromotion(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_learning_promotion_dependencies(
 job_id TEXT PRIMARY KEY,pattern_id TEXT NOT NULL,match_id TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)));
CREATE INDEX IF NOT EXISTS df_learning_promotion_pattern ON df_learning_promotion_dependencies(pattern_id,job_id);
CREATE INDEX IF NOT EXISTS df_learning_promotion_match ON df_learning_promotion_dependencies(match_id);
CREATE TABLE IF NOT EXISTS df_learning_owned_versions(
 source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,
 PRIMARY KEY(source_key,name,version));
CREATE TABLE IF NOT EXISTS df_learning_new_skill_consumed(
 digest TEXT PRIMARY KEY,job_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_learning_new_skill_successors(
 digest TEXT NOT NULL,previous_job TEXT PRIMARY KEY,next_job TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS df_learning_new_skill_retry_spent(
 digest TEXT NOT NULL,ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 3),job_id TEXT NOT NULL UNIQUE,
 previous_digest TEXT NOT NULL UNIQUE,PRIMARY KEY(digest,ordinal));
CREATE TABLE IF NOT EXISTS df_learning_new_skill_retries(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 pattern_id TEXT NOT NULL REFERENCES df_learning_patterns(id) ON DELETE CASCADE,
 job_id TEXT NOT NULL UNIQUE,request_key TEXT NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL,
 UNIQUE(session_id,request_key));`)
	return err
}

// Keep the current creation reservation and its immutable successor chain.
// Only fresh independent evidence can advance a failed automatic attempt;
// explicit rejection, deletion and adoption are never refunded.
func (s *Store) ReserveNewPromotionTx(ctx context.Context, tx *sql.Tx, pattern, job string) error {
	if err := s.GuardNewPromotionTx(ctx, tx, pattern, ""); err != nil {
		return err
	}
	digest := encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern})
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_new_skill_consumed WHERE digest=?`, digest).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_new_skill_successors VALUES(?,?,?)`, digest, previous, job); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO df_learning_new_skill_consumed(digest,job_id) VALUES(?,?) ON CONFLICT(digest) DO UPDATE SET job_id=excluded.job_id`, digest, job)
	return err
}

func (s *Store) GuardNewPromotionTx(ctx context.Context, tx *sql.Tx, pattern, job string) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_new_skill_consumed WHERE digest=?`, encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern})).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) && job == "" {
		return nil
	}
	if err == nil && job == "" {
		// The membership ledger is host-issued. A renamed pattern or a repeated
		// root cannot unlock generation; two new roots must postdate the prior
		// generation's pinned revision. Discovery also spends their episode IDs.
		var fresh bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs j
          JOIN df_skill_generation_inputs g ON g.job_id=j.id
          JOIN df_skill_discoveries d ON d.job_id=j.id
          LEFT JOIN df_skill_proposals p ON p.id=g.proposal_id
          WHERE j.id=? AND j.state='failed' AND (p.id IS NULL OR p.status='failed')
          AND NOT EXISTS(SELECT 1 FROM df_skill_publications h WHERE json_extract(CAST(h.payload AS TEXT),'$.source_proposal_id')=p.id)
          AND (SELECT count(DISTINCT m.independence_key) FROM df_learning_pattern_members m
               WHERE m.pattern_id=? AND m.revision>json_extract(CAST(d.payload AS TEXT),'$.evidence[0].experience.pattern_revision'))>=2)`, existing, pattern).Scan(&fresh)
		if err != nil {
			return err
		}
		if fresh {
			return nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && (job == "" || existing != job) {
		if err == nil && job != "" {
			_, retryErr := s.newSkillRetryTx(ctx, tx, pattern, job, true)
			return retryErr
		}
		return invalid("new capability pattern already consumed or missing reservation")
	}
	return err
}

func (s *Store) BindPromotionTx(ctx context.Context, tx *sql.Tx, job, match, pattern string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_promotion_dependencies(job_id,pattern_id,match_id) VALUES(?,?,?)`, job, pattern, match)
	return err
}

func (s *Store) GuardPromotionTx(ctx context.Context, tx *sql.Tx, job, match, pattern string) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_promotion_dependencies
 WHERE job_id=? AND match_id=? AND pattern_id=? AND revoked=0)`, job, match, pattern).Scan(&valid)
	if err == nil && !valid {
		return invalid("revoked or missing promotion provenance")
	}
	return err
}
