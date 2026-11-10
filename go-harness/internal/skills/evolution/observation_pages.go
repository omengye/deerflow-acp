package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Samples are current projections of source signals, not additional votes.
// Source/feedback updates invalidate them transactionally. A bounded scan can
// accumulate failures immediately but cannot stabilize an incomplete census.
func (s *Service) collectObservationPageTx(ctx context.Context, tx *sql.Tx, o *harness.SkillAutoPatchObservation) (complete, critical bool, err error) {
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_monitor_cursor VALUES(?,'')`, o.ProposalID); err != nil {
		return
	}
	var cursor string
	if err = tx.QueryRowContext(ctx, `SELECT cursor FROM df_skill_monitor_cursor WHERE proposal_id=?`, o.ProposalID).Scan(&cursor); err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_signals WHERE source_key=? AND name=? AND version=? AND id>? ORDER BY id LIMIT 128`, o.Target.SourceIdentity, o.Target.Name, o.Head.Version, cursor)
	if err != nil {
		return
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
		return
	}
	allowed := map[string]harness.SkillTarget{o.Target.SourceIdentity: o.Target}
	for _, id := range ids {
		signal, e := readSignal(ctx, tx, id, allowed)
		if e != nil {
			err = e
			return
		}
		q := signal.Quality
		observed := time.Time{}
		inWindow := true
		refs := q.EvidenceReadIDs
		if len(refs) == 0 {
			var at string
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(r.observed_at),'') FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id WHERE l.signal_id=?`, id).Scan(&at); err != nil {
				return
			}
			observed, e = time.Parse(time.RFC3339Nano, at)
			inWindow = e == nil && !observed.Before(o.StartedAt) && (o.Status == "stable" || !observed.After(o.Deadline))
		} else {
			for _, ref := range refs {
				var at string
				if err = tx.QueryRowContext(ctx, `SELECT r.observed_at FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id WHERE l.signal_id=? AND r.id=?`, id, ref).Scan(&at); err != nil {
					return
				}
				value, e := time.Parse(time.RFC3339Nano, at)
				if e != nil {
					err = errCollectionEvidence
					return
				}
				inWindow = inWindow && !value.Before(o.StartedAt) && (o.Status == "stable" || !value.After(o.Deadline))
				if value.After(observed) {
					observed = value
				}
			}
		}
		outcome, isCritical := "outside", false
		if inWindow {
			outcome = "ignored"
			if q.Eligible && q.Confidence == "high" && (q.Source == "operator_confirmed" || q.Source == "host_verified") {
				if q.Kind == "skill_quality_success" {
					outcome = "success"
				}
				if q.Kind == "skill_quality_failure" || q.Kind == "skill_quality_critical_failure" {
					outcome = "failure"
					isCritical = q.Kind == "skill_quality_critical_failure"
				}
			}
			payload, e := json.Marshal(q)
			if e != nil {
				err = e
				return
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_auto_patch_evidence VALUES(?,?,?,?,?,?) ON CONFLICT(proposal_id,root_id) DO UPDATE SET signal_id=excluded.signal_id,feedback_revision=excluded.feedback_revision,outcome=excluded.outcome,assessment=excluded.assessment`, o.ProposalID, signal.RootID, id, q.FeedbackRevision, outcome, payload); err != nil {
				return
			}
		} else {
			if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_auto_patch_evidence WHERE proposal_id=? AND root_id=?`, o.ProposalID, signal.RootID); err != nil {
				return
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_monitor_samples VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(proposal_id,root_id) DO UPDATE SET signal_id=excluded.signal_id,state_version=excluded.state_version,feedback_revision=excluded.feedback_revision,outcome=excluded.outcome,observed_ns=excluded.observed_ns,critical=excluded.critical`, o.ProposalID, signal.RootID, id, signal.StateVersion, q.FeedbackRevision, outcome, observed.UnixNano(), isCritical); err != nil {
			return
		}
		cursor = id
	}
	if len(ids) < 128 {
		cursor = ""
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_monitor_cursor SET cursor=? WHERE proposal_id=?`, cursor, o.ProposalID); err != nil {
		return
	}
	if err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM df_skill_signals s WHERE s.source_key=? AND s.name=? AND s.version=? AND NOT EXISTS(SELECT 1 FROM df_skill_monitor_samples m WHERE m.proposal_id=? AND m.signal_id=s.id AND m.state_version=s.state_version))`, o.Target.SourceIdentity, o.Target.Name, o.Head.Version, o.ProposalID).Scan(&complete); err != nil {
		return
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(outcome='success'),0),COALESCE(sum(outcome='failure'),0),COALESCE(sum(outcome='ignored'),0),COALESCE(max(critical),0) FROM df_skill_monitor_samples WHERE proposal_id=?`, o.ProposalID).Scan(&o.Successes, &o.Failures, &o.Ignored, &critical); err != nil {
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_monitor_samples m WHERE m.proposal_id=? AND m.outcome='failure' AND NOT EXISTS(SELECT 1 FROM df_skill_monitor_samples s WHERE s.proposal_id=m.proposal_id AND s.outcome='success' AND (s.observed_ns>m.observed_ns OR (s.observed_ns=m.observed_ns AND s.root_id>m.root_id)))`, o.ProposalID).Scan(&o.ConsecutiveFailures)
	return
}
