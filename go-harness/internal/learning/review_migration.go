package learning

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func migrateReview(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_learning_pattern_reviews(
 pattern_id TEXT PRIMARY KEY REFERENCES df_learning_patterns(id) ON DELETE CASCADE,
 state_version INTEGER NOT NULL CHECK(state_version>=1),ignored INTEGER NOT NULL CHECK(ignored IN (0,1)),
 adjudication_revision INTEGER NOT NULL DEFAULT 0,adjudication_id TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS df_learning_adjudications(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 pattern_id TEXT NOT NULL,revision INTEGER NOT NULL,state_version INTEGER NOT NULL,
 policy_version TEXT NOT NULL,model_identity TEXT NOT NULL,access_identity TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,result BLOB NOT NULL,result_sha TEXT NOT NULL,created_at TEXT NOT NULL,
 FOREIGN KEY(pattern_id,revision) REFERENCES df_learning_pattern_revisions(pattern_id,revision) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS df_learning_adjudication_session ON df_learning_adjudications(session_id,id);
CREATE TABLE IF NOT EXISTS df_learning_review_receipts(
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,request_key TEXT NOT NULL,
 pattern_id TEXT NOT NULL REFERENCES df_learning_patterns(id) ON DELETE CASCADE,
 request_sha TEXT NOT NULL,result BLOB NOT NULL,result_sha TEXT NOT NULL,
 PRIMARY KEY(session_id,request_key));`)
	return err
}

func patternReviewTx(ctx context.Context, tx *sql.Tx, pattern string) (harness.SkillLearningPatternReview, error) {
	var review harness.SkillLearningPatternReview
	err := tx.QueryRowContext(ctx, `SELECT p.id,p.current_revision,COALESCE(r.state_version,0),COALESCE(r.ignored,0),
 COALESCE(r.adjudication_revision,0),COALESCE(r.adjudication_id,'') FROM df_learning_patterns p
 LEFT JOIN df_learning_pattern_reviews r ON r.pattern_id=p.id WHERE p.id=?`, pattern).Scan(
		&review.PatternID, &review.PatternRevision, &review.StateVersion, &review.Ignored, &review.AdjudicationRevision, &review.AdjudicationID)
	if err == nil && (review.PatternRevision < 1 || review.StateVersion < 0 || review.AdjudicationRevision < 0 ||
		(review.AdjudicationRevision == 0) != (review.AdjudicationID == "") || review.AdjudicationRevision > review.PatternRevision) {
		err = invalid("inconsistent pattern review")
	}
	return review, err
}
