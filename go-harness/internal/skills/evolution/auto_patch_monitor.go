package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
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
		if o.Status != "observing" {
			return nil
		}
		if tx.Target() != o.Target {
			return harness.ErrPermissionDenied
		}
		now := s.autoPatch.now().UTC()
		if now.Before(o.StartedAt) {
			return invalid("auto_patch observation clock moved backwards")
		}
		current, err := tx.Current()
		if err != nil {
			return err
		}
		o.UpdatedAt = now
		if err = refreshAutoPatchLearningSource(ctx, tx.SQL(), &o); err != nil {
			return err
		}
		if current != o.Head {
			o.Status, o.Reason = "superseded", "The installed version changed during observation."
			return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
		}
		// The table's unique root/version key limits each task to one judgment.
		rows, err := tx.SQL().QueryContext(ctx, `SELECT id FROM df_skill_signals WHERE source_key=? AND name=? AND version=? ORDER BY id LIMIT 1001`, o.Target.SourceIdentity, o.Target.Name, o.Head.Version)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var signalID string
			if err = rows.Scan(&signalID); err != nil {
				break
			}
			ids = append(ids, signalID)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		truncated := len(ids) > 1000
		if truncated {
			ids = ids[:1000]
		}
		o.Successes, o.Failures, o.Ignored, o.ConsecutiveFailures = 0, 0, 0, 0
		type observationResult struct {
			root, outcome string
			at            time.Time
		}
		var results []observationResult
		if _, err = tx.SQL().ExecContext(ctx, `DELETE FROM df_skill_auto_patch_evidence WHERE proposal_id=?`, id); err != nil {
			return err
		}
		allowed := map[string]harness.SkillTarget{o.Target.SourceIdentity: o.Target}
		for _, signalID := range ids {
			signal, err := readSignal(ctx, tx.SQL(), signalID, allowed)
			if err != nil {
				return err
			}
			q := signal.Quality
			inWindow := false
			var observedAt time.Time
			// Count only a real read during this publication's observation period.
			// A reused content hash from an older publication is not new evidence.
			refs := q.EvidenceReadIDs
			if len(refs) == 0 {
				var observed string
				if err = tx.SQL().QueryRowContext(ctx, `SELECT COALESCE(MAX(r.observed_at),'') FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id WHERE l.signal_id=?`, signal.ID).Scan(&observed); err != nil {
					return err
				}
				at, e := time.Parse(time.RFC3339Nano, observed)
				observedAt = at
				inWindow = e == nil && !at.Before(o.StartedAt) && !at.After(o.Deadline)
			} else {
				inWindow = true
				for _, ref := range refs {
					var observed string
					if err = tx.SQL().QueryRowContext(ctx, `SELECT r.observed_at FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id WHERE l.signal_id=? AND r.id=?`, signal.ID, ref).Scan(&observed); err != nil {
						return err
					}
					at, e := time.Parse(time.RFC3339Nano, observed)
					if e != nil {
						return errCollectionEvidence
					}
					inWindow = inWindow && !at.Before(o.StartedAt) && !at.After(o.Deadline)
					if at.After(observedAt) {
						observedAt = at
					}
				}
			}
			if !inWindow {
				continue
			}
			outcome := "ignored"
			if q.Eligible && q.Confidence == "high" && q.Source == "operator_confirmed" {
				if q.Kind == "skill_quality_success" {
					outcome = "success"
				}
				if q.Kind == "skill_quality_failure" {
					outcome = "failure"
				}
			}
			switch outcome {
			case "success":
				o.Successes++
			case "failure":
				o.Failures++
			default:
				o.Ignored++
			}
			results = append(results, observationResult{root: signal.RootID, outcome: outcome, at: observedAt})
			payload, e := json.Marshal(q)
			if e != nil {
				return e
			}
			if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_auto_patch_evidence(proposal_id,root_id,signal_id,feedback_revision,outcome,assessment) VALUES(?,?,?,?,?,?)`, id, signal.RootID, signal.ID, q.FeedbackRevision, outcome, payload); err != nil {
				return err
			}
		}
		// Use host read chronology, never random signal IDs or feedback arrival
		// order. Current feedback revises a task's judgment without adding a vote.
		slices.SortFunc(results, func(a, b observationResult) int {
			if cmp := a.at.Compare(b.at); cmp != 0 {
				return cmp
			}
			return strings.Compare(a.root, b.root)
		})
		for _, result := range results {
			switch result.outcome {
			case "failure":
				o.ConsecutiveFailures++
			case "success":
				o.ConsecutiveFailures = 0
			}
		}
		if !truncated && o.ConsecutiveFailures >= o.Options.RollbackFailureThreshold {
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
			next, err := tx.Publish(baseline.ContentVersion, current, baseline.Enabled)
			if err != nil {
				return err
			}
			rollback := Publication{Action: "rollback", Target: o.Target, Name: o.Target.Name, ContentVersion: baseline.ContentVersion, Enabled: baseline.Enabled, Deleted: baseline.Deleted, Previous: current, Head: next, Actor: "auto_patch_monitor", Note: "Consecutive confirmed skill-quality failures reached the observation threshold.", RollbackOf: baseline.Revision, CreatedAt: now}
			rollback.OverlayBase, err = tx.OverlayBase()
			if err != nil {
				return err
			}
			rollback, err = insertPublication(ctx, tx, rollback, "auto_patch_rollback/"+id, encodedHash(o))
			if err != nil {
				return err
			}
			o.Status, o.Reason, o.RollbackRevision = "rolled_back", rollback.Note, rollback.Revision
		} else if !now.Before(o.Deadline) {
			o.Status, o.Reason = "insufficient_evidence", "Observation ended without enough confirmed quality assessments."
			if !truncated && o.Successes+o.Failures >= o.Options.MinObservations {
				o.Status, o.Reason = "passed", "Observation completed below the rollback failure threshold."
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
 PRIMARY KEY(proposal_id,root_id));`)
	return err
}
