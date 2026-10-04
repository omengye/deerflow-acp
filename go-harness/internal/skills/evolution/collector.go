package evolution

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var errCollectionEvidence = errors.New("skill collection evidence is inconsistent")

type collectorLifecycle struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
}

func signalID(root, source, name, version string) string {
	return "sig_" + encodedHash([]string{"skill-use-v1", root, source, name, version})
}

type collectionRead struct {
	id                                           int64
	root, sourceID, source, scope, name, version string
	path, hash, attempt, role, kind, session     string
	bytes                                        int64
}

// CollectEvidence commits at most one bounded read page. It is independent of
// model generation/publication and may be used by a deterministic host worker.
func (s *Service) CollectEvidence(ctx context.Context, limit int) (bool, error) {
	if limit < 1 || limit > 256 {
		return false, invalid("collection page must be 1..256")
	}
	claim, ok, err := s.claimCollection(ctx, time.Now())
	if err != nil || !ok {
		return false, err
	}
	err = s.collectPage(ctx, claim, limit, time.Now())
	if err != nil {
		code := "storage_error"
		if errors.Is(err, errCollectionEvidence) {
			code = "invalid_evidence"
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		err = errors.Join(err, s.failCollection(cleanup, claim, code, errors.Is(ctx.Err(), context.Canceled), time.Now()))
	}
	return true, err
}

func (s *Service) collectPage(ctx context.Context, claim collectionClaim, limit int, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire writer ownership and reject stale/expired workers before reading
	// any source evidence. The entire page, its cursor and ACK are atomic.
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET token=token WHERE outcome_seq=? AND state='running' AND state_version=? AND token=? AND lease_ns>?`, claim.sequence, claim.version, claim.token, now.UnixNano())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return harness.ErrSkillConflict
	}
	var run, session, status string
	var uncertain bool
	var processed int
	err = tx.QueryRowContext(ctx, `SELECT run_id,session_id,status,effects_uncertain,processed FROM harness_skill_outcomes WHERE sequence=?`, claim.sequence).Scan(&run, &session, &status, &uncertain, &processed)
	if err != nil {
		return err
	}
	if run != claim.run || session != claim.session || status != claim.status || uncertain != claim.uncertain || processed != 0 || (status != "completed" && status != "failed" && status != "cancelled") {
		return errCollectionEvidence
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,root_id,source_id,source_key,scope,name,version,path,file_hash,bytes,attempt_id,role_id,kind,session_id FROM harness_skill_reads WHERE run_id=? AND id>? ORDER BY id LIMIT ?`, run, claim.cursor, limit+1)
	if err != nil {
		return err
	}
	var reads []collectionRead
	for rows.Next() {
		var r collectionRead
		if err = rows.Scan(&r.id, &r.root, &r.sourceID, &r.source, &r.scope, &r.name, &r.version, &r.path, &r.hash, &r.bytes, &r.attempt, &r.role, &r.kind, &r.session); err != nil {
			break
		}
		reads = append(reads, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	more := len(reads) > limit
	if more {
		reads = reads[:limit]
	}
	for _, r := range reads {
		if r.session != session || r.id <= claim.cursor || !harness.ValidSkillName(r.sourceID) || !harness.ValidSkillName(r.name) || !hexDigest(r.source) || !hexDigest(r.version) || !hexDigest(r.hash) ||
			(r.scope != "workspace" && r.scope != "global") || (r.kind != "skill_load" && r.kind != "support_read") || r.kind == "skill_load" && r.path != "SKILL.md" || !bounded(r.path, 1024) || r.path == "" || r.bytes < 0 {
			return errCollectionEvidence
		}
		var root, rootSession, memberSession, attemptRoot, attemptRun, attemptSession string
		err = tx.QueryRowContext(ctx, `SELECT m.root_id,b.session_id,m.session_id,a.root_id,a.member_id,a.session_id
 FROM budget_members m JOIN budget_roots b ON b.id=m.root_id JOIN budget_attempts a ON a.id=? WHERE m.id=?`, r.attempt, run).Scan(&root, &rootSession, &memberSession, &attemptRoot, &attemptRun, &attemptSession)
		if errors.Is(err, sql.ErrNoRows) {
			return errCollectionEvidence
		}
		if err != nil {
			return err
		}
		if root != r.root || attemptRoot != root || attemptRun != run || memberSession != session || attemptSession != session {
			return errCollectionEvidence
		}
		var fileHash string
		var bytes int64
		err = tx.QueryRowContext(ctx, `SELECT hash,size FROM df_skill_files WHERE source_key=? AND name=? AND version=? AND path=?`, r.source, r.name, r.version, r.path).Scan(&fileHash, &bytes)
		if errors.Is(err, sql.ErrNoRows) {
			return errCollectionEvidence
		}
		if err != nil {
			return err
		}
		if fileHash != r.hash || bytes != r.bytes {
			return errCollectionEvidence
		}
		id := signalID(root, r.source, r.name, r.version)
		_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_signals(id,root_id,session_id,source_id,source_key,scope,name,version) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(root_id,source_key,name,version) DO NOTHING`, id, root, rootSession, r.sourceID, r.source, r.scope, r.name, r.version)
		if err != nil {
			return err
		}
		var savedID, savedSession, savedSourceID, savedScope string
		err = tx.QueryRowContext(ctx, `SELECT id,session_id,source_id,scope FROM df_skill_signals WHERE root_id=? AND source_key=? AND name=? AND version=?`, root, r.source, r.name, r.version).Scan(&savedID, &savedSession, &savedSourceID, &savedScope)
		if err != nil {
			return err
		}
		if savedID != id || savedSession != rootSession || savedSourceID != r.sourceID || savedScope != r.scope {
			return errCollectionEvidence
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_signal_reads(read_id,signal_id,outcome_seq) VALUES(?,?,?) ON CONFLICT(read_id) DO NOTHING`, r.id, id, claim.sequence)
		if err != nil {
			return err
		}
		var linkedID string
		var sequence int64
		if err = tx.QueryRowContext(ctx, `SELECT signal_id,outcome_seq FROM df_skill_signal_reads WHERE read_id=?`, r.id).Scan(&linkedID, &sequence); err != nil {
			return err
		}
		if linkedID != id || sequence != claim.sequence {
			return errCollectionEvidence
		}
		claim.cursor = r.id
	}
	state := "pending"
	if !more {
		state = "completed"
		if _, err = tx.ExecContext(ctx, `UPDATE harness_skill_outcomes SET processed=1 WHERE sequence=? AND processed=0`, claim.sequence); err != nil {
			return err
		}
		// ACK changes the signal's collection-pending projection as well.
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_signal_catalog SET revision=revision+1 WHERE id=1`); err != nil {
			return err
		}
	}
	result, err = tx.ExecContext(ctx, `UPDATE df_skill_collection_jobs SET state=?,read_cursor=?,state_version=state_version+1,attempts=0,token='',lease_ns=0,retry_ns=0,error_code='' WHERE outcome_seq=? AND state='running' AND state_version=? AND token=? AND lease_ns>?`, state, claim.cursor, claim.sequence, claim.version, claim.token, time.Now().UnixNano())
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return harness.ErrSkillConflict
	}
	return tx.Commit()
}

// StartCollector is called only after runtime/evidence tables exist. This local
// worker has no models or external tools; generation gets its own budget later.
func (s *Service) StartCollector() error {
	s.collector.mu.Lock()
	defer s.collector.mu.Unlock()
	if s.collector.closed {
		return errors.New("skill collector is closed")
	}
	if s.collector.done != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.collector.cancel, s.collector.done = cancel, make(chan struct{})
	go func() {
		defer close(s.collector.done)
		warned := false
		for {
			step, stop := context.WithTimeout(ctx, 2*time.Second)
			worked, err := s.CollectEvidence(step, 128)
			stop()
			if ctx.Err() != nil {
				return
			}
			delay := time.Second
			if err != nil {
				if !warned {
					slog.Warn("skill evidence collection failed; task outcomes remain committed")
				}
				warned, delay = true, 5*time.Second
			} else {
				warned = false
				if worked {
					delay = 10 * time.Millisecond
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

// Close cancels and joins generation, then the collector, before the owner
// closes its database. Other service operations are joined by the SDK lifecycle.
func (s *Service) Close() error {
	s.closeAutoPatch()
	_ = s.CloseDiscovery()
	s.closeGeneration()
	s.collector.mu.Lock()
	s.collector.closed = true
	if s.collector.cancel != nil {
		s.collector.cancel()
	}
	done := s.collector.done
	s.collector.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}
