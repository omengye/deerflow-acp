package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// Reuse the candidate, never its obsolete verdict. Archived reports/results
// remain audit evidence; each environment gets one durable evaluation job.
func (s *Service) recheckEnvironment(ctx context.Context, id string) (queued bool, err error) {
	if s.behavior == nil {
		return false, nil
	}
	err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.PublicationPolicy == harness.SkillPublicationSecurityOnly || (p.Status != "pending_review" && p.Status != "validating" && p.Status != "failed") || p.Origin != "automatic" {
			return nil
		}
		var retired bool
		if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_evolution_retired_proposals WHERE proposal_id=?)`, id).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return nil
		}
		old, err := readBehavior(ctx, tx.SQL(), id)
		if errors.Is(err, harness.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var waiting *skillmaintenance.Job
		if p.Status != "pending_review" {
			job, e := skillmaintenance.ReadJob(ctx, tx.SQL(), old.jobID)
			if e != nil {
				return e
			}
			if job.State == "waiting" && (job.ErrorCode == "blocked_evidence" || job.ErrorCode == "blocked_runtime") {
				waiting = &job
			} else if job.State != "failed" || job.ErrorCode != "configuration_changed" {
				return nil
			}
		}
		changed := old.input.EvaluatorIdentity != s.behavior.identity || old.input.ValidatorIdentity != s.generation.reviewIdentity
		if old.input.Derived {
			_, catalog, e := s.behaviorCompetitorsTx(ctx, tx.SQL(), *p)
			if e != nil {
				return e
			}
			changed = changed || catalog != old.input.CompetitorsDigest
		} else {
			changed = changed || old.input.SuiteDigest != s.behavior.suites[behaviorKey(p.Target)].digest
		}
		if !changed {
			return nil
		}
		if head, e := tx.Current(); e != nil {
			return e
		} else if head != p.Base {
			return nil
		}
		in := s.newBehaviorSnapshot(*p)
		in.Submission = old.input.Submission
		if in.Suite == nil {
			if err := s.deriveBehaviorCasesTx(ctx, tx.SQL(), *p, &in); err != nil {
				return err
			}
		}
		if err = persistBehaviorCases(ctx, tx.SQL(), &in); err != nil {
			return err
		}
		if waiting != nil {
			// No lease is active. Retire only this obsolete environment's job;
			// the immutable candidate and its original provenance stay intact.
			result, e := tx.SQL().ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='failed',state_version=state_version+1,error_code='configuration_changed',retry_ns=0 WHERE id=? AND state='waiting' AND state_version=? AND lease_ns=0`, waiting.ID, waiting.StateVersion)
			if e != nil {
				return e
			}
			if n, e := result.RowsAffected(); e != nil {
				return e
			} else if n != 1 {
				return harness.ErrSkillConflict
			}
		}
		// Archive the old immutable binding before replacing the active evaluation.
		if err = archiveBehavior(ctx, tx.SQL(), old); err != nil {
			return err
		}
		raw, _ := json.Marshal(in)
		job, err := s.generation.store.EnqueueTx(ctx, tx.SQL(), skillmaintenance.Request{Kind: skillmaintenance.SkillBehavior, SourceIdentity: p.Target.SourceIdentity, Name: p.Target.Name, Key: "environment/" + id + "/" + old.input.ID + "/" + encodedHash([]string{in.EvaluatorIdentity, in.ValidatorIdentity, in.SuiteDigest, in.CompetitorsDigest}), Reference: in.ID, InputDigest: hash(raw)})
		if err != nil {
			return err
		}
		if err = insertBehavior(ctx, tx.SQL(), in, job.ID, true); err != nil {
			return err
		}
		// Suite-only changes do not invalidate content review of the same
		// package by the same validator. Behavior results are never reused.
		reuseContent := old.content != nil && old.input.ValidatorIdentity == in.ValidatorIdentity
		if reuseContent {
			content, e := json.Marshal(old.content)
			if e != nil {
				return e
			}
			if _, e = tx.SQL().ExecContext(ctx, `UPDATE df_skill_behavior_evaluations SET content_report=?,content_sha=? WHERE id=?`, content, hash(content), in.ID); e != nil {
				return e
			}
		}
		before := p.StateVersion
		p.StateVersion++
		p.Status = "validating"
		p.BehaviorJobID = job.ID
		p.Evaluation = Report{}
		if reuseContent {
			p.Evaluation = *old.content
		}
		p.EvaluationDigest = ""
		if reuseContent {
			p.EvaluationDigest = encodedHash(p.Evaluation)
		}
		p.UpdatedAt = time.Now().UTC()
		p.Error = ""
		p.ValidationFailure = nil
		queued = true
		return updateProposal(ctx, tx.SQL(), *p, before)
	})
	return
}

func archiveBehavior(ctx context.Context, tx *sql.Tx, old behaviorRecord) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_behavior_history SELECT * FROM df_skill_behavior_evaluations WHERE id=?`, old.input.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_behavior_result_history SELECT * FROM df_skill_behavior_results WHERE evaluation_id=?`, old.input.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM df_skill_behavior_results WHERE evaluation_id=?`, old.input.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM df_skill_behavior_evaluations WHERE id=?`, old.input.ID)
	return err
}
