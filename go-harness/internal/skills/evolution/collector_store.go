package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const collectionLease = 10 * time.Second
const maxCollectionAttempts = 3

func migrateCollector(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_collection_jobs (
 outcome_seq INTEGER PRIMARY KEY REFERENCES harness_skill_outcomes(sequence) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 state_version INTEGER NOT NULL DEFAULT 1,state TEXT NOT NULL DEFAULT 'pending',
 read_cursor INTEGER NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,
 token TEXT NOT NULL DEFAULT '',lease_ns INTEGER NOT NULL DEFAULT 0,
 retry_ns INTEGER NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS df_skill_collection_ready ON df_skill_collection_jobs(state,retry_ns,outcome_seq);
CREATE TABLE IF NOT EXISTS df_skill_signal_catalog(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL);
INSERT OR IGNORE INTO df_skill_signal_catalog VALUES(1,0);
CREATE TABLE IF NOT EXISTS df_skill_signals (
 id TEXT PRIMARY KEY,root_id TEXT NOT NULL,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 source_id TEXT NOT NULL,source_key TEXT NOT NULL,scope TEXT NOT NULL,
 name TEXT NOT NULL,version TEXT NOT NULL,state_version INTEGER NOT NULL DEFAULT 1,
 UNIQUE(root_id,source_key,name,version));
CREATE INDEX IF NOT EXISTS df_skill_signals_source ON df_skill_signals(source_key,name,id);
CREATE TABLE IF NOT EXISTS df_skill_signal_reads (
 read_id INTEGER PRIMARY KEY REFERENCES harness_skill_reads(id) ON DELETE CASCADE,
 signal_id TEXT NOT NULL REFERENCES df_skill_signals(id) ON DELETE CASCADE,
 outcome_seq INTEGER NOT NULL REFERENCES harness_skill_outcomes(sequence) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS df_skill_signal_reads_signal ON df_skill_signal_reads(signal_id,read_id);
CREATE TRIGGER IF NOT EXISTS df_skill_signal_created AFTER INSERT ON df_skill_signals
 BEGIN UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_signal_changed AFTER UPDATE ON df_skill_signals
 BEGIN UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_signal_deleted AFTER DELETE ON df_skill_signals
 BEGIN UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_signal_read_added AFTER INSERT ON df_skill_signal_reads
 BEGIN UPDATE df_skill_signals SET state_version=state_version+1 WHERE id=NEW.signal_id; END;
CREATE TRIGGER IF NOT EXISTS df_skill_signal_read_deleted AFTER DELETE ON df_skill_signal_reads
 BEGIN UPDATE df_skill_signals SET state_version=state_version+1 WHERE id=OLD.signal_id; END;`)
	return err
}

type collectionClaim struct {
	sequence, version, cursor   int64
	attempts                    int
	token, run, session, status string
	uncertain                   bool
}

// Claims and page progress are durable. An expired collector can neither commit
// a page nor fail/retry a replacement worker's job. No model runs in this lease.
func (s *Service) claimCollection(ctx context.Context, now time.Time) (collectionClaim, bool, error) {
	var claim collectionClaim
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return claim, false, err
	}
	defer tx.Rollback()
	// This first write acquires SQLite's writer slot before reading job state.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_collection_jobs(outcome_seq,session_id)
 SELECT o.sequence,o.session_id FROM harness_skill_outcomes o
 WHERE o.processed=0 AND NOT EXISTS(SELECT 1 FROM df_skill_collection_jobs j WHERE j.outcome_seq=o.sequence)
 ORDER BY o.sequence LIMIT 32`)
	if err != nil {
		return claim, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET state=CASE WHEN attempts>=? THEN 'failed' ELSE 'retry' END,
 state_version=state_version+1,token='',lease_ns=0,retry_ns=?,error_code='interrupted'
 WHERE state='running' AND lease_ns<=?`, maxCollectionAttempts, now.UnixNano(), now.UnixNano())
	if err != nil {
		return claim, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT j.outcome_seq,j.state_version,j.read_cursor,j.attempts,o.run_id,o.session_id,o.status,o.effects_uncertain
 FROM df_skill_collection_jobs j JOIN harness_skill_outcomes o ON o.sequence=j.outcome_seq
 WHERE o.processed=0 AND j.state IN ('pending','retry') AND j.retry_ns<=?
 ORDER BY j.outcome_seq LIMIT 1`, now.UnixNano()).Scan(&claim.sequence, &claim.version, &claim.cursor, &claim.attempts, &claim.run, &claim.session, &claim.status, &claim.uncertain)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, false, tx.Commit()
	}
	if err != nil {
		return claim, false, err
	}
	if claim.sequence < 1 || claim.version < 1 || claim.cursor < 0 || claim.attempts < 0 || claim.attempts >= maxCollectionAttempts {
		return claim, false, errCollectionEvidence
	}
	claim.version++
	claim.attempts++
	claim.token = rand.Text()
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET state='running',state_version=?,attempts=?,token=?,lease_ns=?,error_code='' WHERE outcome_seq=?`, claim.version, claim.attempts, claim.token, now.Add(collectionLease).UnixNano(), claim.sequence)
	if err != nil {
		return claim, false, err
	}
	return claim, true, tx.Commit()
}

func (s *Service) failCollection(ctx context.Context, claim collectionClaim, code string, cancelled bool, now time.Time) error {
	state, attempts, retry := "retry", claim.attempts, now.Add(time.Duration(1<<uint(claim.attempts-1))*time.Second).UnixNano()
	if cancelled {
		state, attempts, retry, code = "pending", claim.attempts-1, 0, ""
	} else if attempts >= maxCollectionAttempts {
		state = "failed"
		retry = 0
	}
	_, err := s.db.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET state=?,attempts=?,state_version=state_version+1,token='',lease_ns=0,retry_ns=?,error_code=?
 WHERE outcome_seq=? AND state='running' AND state_version=? AND token=?`, state, attempts, retry, code, claim.sequence, claim.version, claim.token)
	return err
}

func (s *Service) RetryCollection(ctx context.Context, outcomeID, expectedVersion int64) error {
	if outcomeID < 1 || expectedVersion < 1 {
		return invalid("collection retry requires an outcome and version")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET state='pending',state_version=state_version+1,attempts=0,retry_ns=0,error_code='',token='',lease_ns=0
 WHERE outcome_seq=? AND state_version=? AND state='failed'`, outcomeID, expectedVersion)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return harness.ErrSkillConflict
	}
	return err
}
