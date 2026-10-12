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

// A new-capability pattern gets one creation reservation, independent of its
// later revisions, suggested names or configured sources. Keep only a digest
// and accounting reference after forget; do not refund failed/rejected work.
func (s *Store) ReserveNewPromotionTx(ctx context.Context, tx *sql.Tx, pattern, job string) error {
	if err := s.GuardNewPromotionTx(ctx, tx, pattern, ""); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_new_skill_consumed(digest,job_id) VALUES(?,?)`, encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern}), job)
	return err
}

func (s *Store) GuardNewPromotionTx(ctx context.Context, tx *sql.Tx, pattern, job string) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_new_skill_consumed WHERE digest=?`, encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern})).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) && job == "" {
		return nil
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
