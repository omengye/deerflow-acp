package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"time"
)

// Dependency probes perform metadata work only. A failed probe cannot consume
// model calls or reset attempts. New evidence wakes only affected candidates.
func (s *Service) refreshWaiting(ctx context.Context) error {
	g := s.generation
	g.dependencyMu.Lock()
	defer g.dependencyMu.Unlock()
	if time.Since(g.dependencyProbe) < time.Minute {
		return nil
	}
	g.dependencyProbe = time.Now()
	if !s.securityPublicationEnabled() {
		if err := s.refreshBehaviorDependencies(ctx); err != nil {
			return err
		}
	}
	return s.refreshCandidateCapacity(ctx)
}

func (s *Service) refreshBehaviorDependencies(ctx context.Context) error {
	g := s.generation
	var runtimeWait bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE state='waiting' AND error_code='blocked_runtime')`).Scan(&runtimeWait); err != nil {
		return err
	}
	if runtimeWait {
		var err error
		if skillbehavior.RuntimeReady(ctx) {
			err = g.store.WakeChanged(ctx, "blocked_runtime", skillbehavior.RuntimeVersion)
		} else {
			err = g.store.ObserveDependency(ctx, "blocked_runtime", "unavailable")
		}
		if err != nil {
			return err
		}
	}
	var cursor string
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_skill_dependency_scan(id INTEGER PRIMARY KEY,cursor TEXT NOT NULL); INSERT OR IGNORE INTO df_skill_dependency_scan VALUES(1,'')`); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT cursor FROM df_skill_dependency_scan WHERE id=1`).Scan(&cursor); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.proposal_id FROM df_skill_behavior_evaluations e JOIN df_skill_maintenance_jobs j ON j.id=e.job_id WHERE e.proposal_id>? AND j.state='waiting' AND j.error_code='blocked_evidence' AND NOT EXISTS(SELECT 1 FROM df_evolution_retired_proposals p WHERE p.proposal_id=e.proposal_id) ORDER BY e.proposal_id LIMIT 16`, cursor)
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
		if _, err = s.db.ExecContext(ctx, `UPDATE df_skill_dependency_scan SET cursor=? WHERE id=1`, id); err != nil {
			return err
		}
		// A suite loaded on restart is a changed validation environment. Use
		// the existing archival/recheck path before probing task evidence.
		if queued, err := s.recheckEnvironment(ctx, id); err != nil {
			if unavailableEvidence(err) {
				continue
			}
			return err
		} else if queued {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = func() error {
			p, err := readProposal(ctx, tx, id)
			if err != nil {
				return err
			}
			if p.Status != "validating" {
				return nil
			}
			r, err := readBehavior(ctx, tx, id)
			if err != nil {
				return err
			}
			if r.input.Suite != nil {
				return nil
			}
			before, err := json.Marshal(r.input)
			if err != nil {
				return err
			}
			if err = s.deriveBehaviorCasesTx(ctx, tx, p, &r.input); err != nil {
				return err
			}
			if err = persistBehaviorCases(ctx, tx, &r.input); err != nil {
				return err
			}
			raw, _ := json.Marshal(r.input)
			if string(raw) == string(before) {
				return nil
			}
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_behavior_evaluations SET payload=?,payload_sha=? WHERE id=?`, raw, hash(raw), r.input.ID); err != nil {
				return err
			}
			if r.manual {
				if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=? AND state='waiting' AND error_code='blocked_evidence'`, hash(raw), r.jobID); err != nil {
					return err
				}
			}
			if r.input.Suite == nil {
				return nil
			}
			_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='queued',state_version=state_version+1,attempts=0,deadline_ns=0,retry_ns=0,error_code='' WHERE id=? AND state='waiting' AND error_code='blocked_evidence'`, r.jobID)
			return err
		}()
		if err == nil {
			err = tx.Commit()
		}
		_ = tx.Rollback()
		if err != nil {
			if unavailableEvidence(err) {
				continue
			}
			return err
		}
	}
	if len(ids) < 16 {
		if _, err = s.db.ExecContext(ctx, `UPDATE df_skill_dependency_scan SET cursor='' WHERE id=1`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) refreshCandidateCapacity(ctx context.Context) error {
	// First compute the reclaimable set, then recheck references on deletion.
	dry, err := s.registry.MaintainCandidates(ctx, 32, false)
	if err != nil {
		return err
	}
	if dry.Candidates > 0 {
		_, err = s.registry.MaintainCandidates(ctx, 32, true)
	}
	if err != nil {
		return err
	}
	var total int64
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(sum(total_bytes),0) FROM df_skill_versions v WHERE NOT EXISTS(SELECT 1 FROM df_skill_content_gc g WHERE g.source_key=v.source_key AND g.name=v.name AND g.version=v.version AND g.state='expired')`).Scan(&total); err != nil {
		return err
	}
	return s.generation.store.WakeChanged(ctx, "deferred_capacity", fmt.Sprintf("%d", total))
}

// Revoked or deleted sources cannot resume, but must not stop dependency work
// for unrelated candidates. Storage and integrity errors still surface.
func unavailableEvidence(err error) bool {
	return errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, errDiscoveryEvidence)
}
