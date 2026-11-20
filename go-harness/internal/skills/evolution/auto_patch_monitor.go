package evolution

import (
	"context"
	"database/sql"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (s *Service) monitorAutoPatch(ctx context.Context, id string) error {
	old, err := readAutoPatchObservation(ctx, s.db, id)
	if err != nil {
		return err
	}
	return s.registry.WithPublication(ctx, old.Target.SourceID, old.Target.Name, func(tx *skills.PublicationTx) error {
		o, err := readAutoPatchObservation(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		if o.Status != "observing" && o.Status != "stable" {
			return nil
		}
		if tx.Target() != o.Target {
			return harness.ErrPermissionDenied
		}
		now := time.Now().UTC()
		if s.autoPatch != nil {
			now = s.autoPatch.now().UTC()
		}
		if now.Before(o.StartedAt) {
			return invalid("auto_patch observation clock moved backwards")
		}
		current, err := tx.Current()
		if err != nil {
			return err
		}
		if pending, e := skills.PublicationPending(ctx, tx.SQL(), o.Target.SourceIdentity, o.Target.Name); e != nil {
			return e
		} else if pending {
			return nil
		}
		o.UpdatedAt = now
		if err = refreshAutoPatchLearningSource(ctx, tx.SQL(), &o); err != nil {
			return err
		}
		if current != o.Head {
			o.Status, o.Reason = "superseded", "The installed version changed during observation."
			return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
		}
		environmentCurrent, err := s.publishedEnvironmentTx(ctx, tx, &o)
		if err != nil {
			return err
		}
		complete, critical, err := s.collectObservationPageTx(ctx, tx.SQL(), &o)
		if err != nil {
			return err
		}
		truncated := !complete
		expired := o.Status == "observing" && !now.Before(o.Deadline)
		withdrawn := o.VerificationState == "source_withdrawn" || o.LearningSource != nil && o.LearningSource.SourceState != "valid_at_publication"
		covered, coverageErr := s.observationCoverageTx(ctx, tx.SQL(), o)
		if coverageErr != nil {
			return coverageErr
		}
		o.BehaviorCovered = covered
		confirmed, baselineFailed := false, false
		if (o.Failures > 0 || !environmentCurrent) && o.PolicyVersion == "skill-evolution-v2" {
			if o.Failures > 0 {
				if err = s.queueRegressionTx(ctx, tx, o); err != nil {
					return err
				}
			}
			if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_regression_checks WHERE proposal_id=? AND state='confirmed' AND json_extract(CAST(payload AS TEXT),'$.verification_identity')=?),EXISTS(SELECT 1 FROM df_skill_regression_checks WHERE proposal_id=? AND state='baseline_failed' AND json_extract(CAST(payload AS TEXT),'$.verification_identity')=?)`, id, o.VerificationIdentity, id, o.VerificationIdentity).Scan(&confirmed, &baselineFailed); err != nil {
				return err
			}
			baselineFailed = baselineFailed || !environmentCurrent && (confirmed || critical || withdrawn || expired || o.Failures >= o.Options.RollbackFailureThreshold)
			if o.Failures > 0 || confirmed || baselineFailed {
				if _, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_routes SET state='quarantined',stable_version=CASE WHEN ? THEN '' ELSE stable_version END WHERE source_key=? AND name=? AND candidate_version=?`, baselineFailed, o.Target.SourceIdentity, o.Target.Name, o.Head.Version); err != nil {
					return err
				}
			}
		}
		enough := environmentCurrent && o.PolicyVersion == "skill-evolution-v2" && covered && !truncated && o.Successes >= o.Options.MinObservations && o.Failures == 0
		if !baselineFailed && (o.Failures >= o.Options.RollbackFailureThreshold || critical || confirmed || withdrawn || expired && !enough) {
			baseline, err := readPublication(ctx, tx, o.BaselineRevision)
			if err != nil {
				return err
			}
			published, err := readPublication(ctx, tx, o.PublicationRevision)
			if err != nil {
				return err
			}
			if published.Head != current || published.Previous != baseline.Head || published.ProposalID != o.ProposalID {
				return harness.ErrSkillConflict
			}
			var scope string
			e := tx.SQL().QueryRowContext(ctx, `SELECT workspace FROM df_skill_routes WHERE source_key=? AND name=? AND candidate_version=?`, o.Target.SourceIdentity, o.Target.Name, current.Version).Scan(&scope)
			var next skills.SkillHead
			if e == nil {
				next, err = tx.PublishAutomatic(baseline.ContentVersion, current, baseline.Enabled, scope)
			} else if e == sql.ErrNoRows {
				next, err = tx.Publish(baseline.ContentVersion, current, baseline.Enabled)
			} else {
				return e
			}
			if err != nil {
				return err
			}
			rollback := Publication{Action: "rollback", Target: o.Target, Name: o.Target.Name, ContentVersion: baseline.ContentVersion, Enabled: baseline.Enabled, Deleted: baseline.Deleted, Previous: current, Head: next, Actor: "auto_patch_monitor", Note: "Candidate withdrawn after cumulative failures, withdrawn evidence, or an inconclusive observation deadline.", RollbackOf: baseline.Revision, CreatedAt: now}
			rollback.OverlayBase, err = tx.OverlayBase()
			if err != nil {
				return err
			}
			rollback, err = insertPublication(ctx, tx, rollback, "auto_patch_rollback/"+id, encodedHash(o))
			if err != nil {
				return err
			}
			o.Status, o.Reason, o.RollbackRevision = "rolled_back", rollback.Note, rollback.Revision
		} else if baselineFailed {
			o.Status, o.Reason = "withdrawn", "The candidate cannot remain in use and no baseline is verified for the current environment."
		} else if enough {
			o.Status, o.Reason = "stable", "Independent successful tasks met the host observation requirements."
		}
		if o.Failures == 0 && o.Status == "observing" {
			if _, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_routes SET state='observing' WHERE source_key=? AND name=? AND candidate_version=?`, o.Target.SourceIdentity, o.Target.Name, o.Head.Version); err != nil {
				return err
			}
		}
		if o.Status != "observing" && (o.Failures == 0 || o.Status == "rolled_back") {
			if _, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_routes SET state=?,stable_version=CASE WHEN ?='stable' THEN candidate_version ELSE stable_version END WHERE source_key=? AND name=? AND candidate_version=?`, o.Status, o.Status, o.Target.SourceIdentity, o.Target.Name, o.Head.Version); err != nil {
				return err
			}
		}

		return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
	})
}

// Retain the host-confirmed assessment revision used by a decision. Feedback
// withdrawal/replacement is reflected on the next scan before any decision.
func migrateAutoPatchEvidence(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_skill_auto_patch_evidence(
 proposal_id TEXT NOT NULL REFERENCES df_skill_auto_patch_observations(proposal_id),root_id TEXT NOT NULL,
 signal_id TEXT NOT NULL,feedback_revision INTEGER NOT NULL,outcome TEXT NOT NULL,assessment BLOB NOT NULL,
 PRIMARY KEY(proposal_id,root_id));
CREATE TABLE IF NOT EXISTS df_skill_monitor_cursor(proposal_id TEXT PRIMARY KEY,cursor TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_monitor_samples(proposal_id TEXT NOT NULL,root_id TEXT NOT NULL,signal_id TEXT NOT NULL,state_version INTEGER NOT NULL,feedback_revision INTEGER NOT NULL,outcome TEXT NOT NULL,observed_ns INTEGER NOT NULL,critical INTEGER NOT NULL,PRIMARY KEY(proposal_id,root_id));
CREATE INDEX IF NOT EXISTS df_skill_monitor_signal ON df_skill_monitor_samples(signal_id);
CREATE TRIGGER IF NOT EXISTS df_skill_monitor_signal_changed AFTER UPDATE ON df_skill_signals BEGIN
 DELETE FROM df_skill_monitor_samples WHERE signal_id=NEW.id;
 DELETE FROM df_skill_auto_patch_evidence WHERE signal_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS df_skill_monitor_signal_deleted AFTER DELETE ON df_skill_signals BEGIN
 DELETE FROM df_skill_monitor_samples WHERE signal_id=OLD.id;
 DELETE FROM df_skill_auto_patch_evidence WHERE signal_id=OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS df_skill_monitor_feedback_changed AFTER INSERT ON df_skill_feedback BEGIN
 DELETE FROM df_skill_monitor_samples WHERE signal_id=NEW.signal_id;
 DELETE FROM df_skill_auto_patch_evidence WHERE signal_id=NEW.signal_id;
END;
CREATE TABLE IF NOT EXISTS df_skill_regression_checks(check_key TEXT PRIMARY KEY,job_id TEXT NOT NULL UNIQUE,proposal_id TEXT NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_regression_results(job_id TEXT NOT NULL,result_key TEXT NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL,PRIMARY KEY(job_id,result_key));`)
	return err
}

// Stop sampling already published versions when switching policies. Only the
// exact, previously approved head may become stable within its original scope.
func (s *Service) finishLegacyObservations(ctx context.Context) error {
	ids, err := s.securityUpgradeIDs(ctx, `SELECT proposal_id FROM df_skill_auto_patch_observations WHERE status='observing'`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
			o, err := readAutoPatchObservation(ctx, tx.SQL(), id)
			if err != nil || o.Status != "observing" {
				return err
			}
			head, err := tx.Current()
			if err != nil {
				return err
			}
			sourceWithdrawn := o.VerificationState == "source_withdrawn"
			o.UpdatedAt, o.VerificationState = time.Now().UTC(), "historical"
			if head != o.Head {
				o.Status, o.Reason = "superseded", "A newer publication replaced the observed version."
				return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
			}
			if err = refreshAutoPatchLearningSource(ctx, tx.SQL(), &o); err != nil {
				return err
			}
			var route string
			var retired bool
			err = tx.SQL().QueryRowContext(ctx, `SELECT state, EXISTS(SELECT 1 FROM df_evolution_retired_proposals WHERE proposal_id=?)
 FROM df_skill_routes WHERE source_key=? AND name=? AND candidate_version=? AND workspace<>''`, id, p.Target.SourceIdentity, p.SkillName, head.Version).Scan(&route, &retired)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			grant, ok := s.generation.allowed[p.Target.SourceID]
			grant.Name = p.SkillName
			hasDecision := p.Status == "published" && p.Approval != nil && p.Approval.Review.Actor == "auto_patch" &&
				o.Target == p.Target && p.PublishedRevision == o.PublicationRevision && head.Version == p.CandidateVersion
			sourceActive := ok && grant == p.Target && !retired && !sourceWithdrawn &&
				(o.LearningSource == nil || o.LearningSource.SourceState == "valid_at_publication")
			if hasDecision && sourceActive && route == "observing" && passedSecurityReview(p.Evaluation, ValidationInput{Proposal: *p}) {
				o.Status, o.Reason = "stable", "The previously security-reviewed publication is active in its original workspace."
			} else {
				o.Status, o.Reason = "withdrawn", "The original scoped publication is no longer eligible for routing."
			}
			// Preserve an existing quarantine or withdrawal, including its baseline.
			if _, err = tx.SQL().ExecContext(ctx, `UPDATE df_skill_routes SET state=?,stable_version=CASE WHEN ?='stable' THEN candidate_version ELSE stable_version END
 WHERE source_key=? AND name=? AND candidate_version=? AND state='observing'`, o.Status, o.Status, p.Target.SourceIdentity, p.SkillName, head.Version); err != nil {
				return err
			}
			return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
		})
		if err != nil && !unavailableEvidence(err) {
			return err
		}
	}
	return nil
}
