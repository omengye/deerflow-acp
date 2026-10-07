package evolution

import (
	"context"
	"database/sql"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// AssessSignalTx consumes a host-loaded signal and current durable evidence.
// Only an explicit operator quality judgment can assign a causal quality result;
// ordinary correction/failure text and successful execution alone cannot.
func AssessSignalTx(ctx context.Context, tx *sql.Tx, signal harness.SkillSignal) (harness.SkillQualityAssessment, error) {
	q := harness.SkillQualityAssessment{Kind: "unassessed", Confidence: "none", Source: "host"}
	var pending, cancelled, uncertain, failed bool
	var blocked string
	err := tx.QueryRowContext(ctx, `SELECT blocked_reason,
 EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id LEFT JOIN harness_skill_outcomes o ON o.run_id=r.id WHERE m.root_id=b.id AND (r.status NOT IN ('completed','failed','cancelled') OR o.sequence IS NULL OR o.processed<>1)),
 EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id WHERE m.root_id=b.id AND r.status='cancelled'),
	 EXISTS(SELECT 1 FROM budget_members m JOIN harness_skill_outcomes o ON o.run_id=m.id WHERE m.root_id=b.id AND o.effects_uncertain<>0),
 EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id WHERE m.root_id=b.id AND r.status='failed')
 FROM budget_roots b WHERE b.id=? AND b.session_id=?`, signal.RootID, signal.RootSessionID).Scan(&blocked, &pending, &cancelled, &uncertain, &failed)
	if err != nil {
		return q, err
	}
	if pending {
		q.Kind = "pending"
		return q, nil
	}
	if uncertain {
		q.Kind = "uncertain_effect"
		q.Confidence = "high"
		return q, nil
	}
	if cancelled {
		q.Kind = "cancelled"
		q.Confidence = "high"
		return q, nil
	}
	if blocked != "" {
		q.Kind = "budget"
		q.Confidence = "high"
		return q, nil
	}
	var code string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT json_extract(CAST(t.receipt AS TEXT),'$.failureCode') FROM harness_tool_receipts t JOIN budget_members m ON m.id=t.run_id WHERE m.root_id=? AND json_extract(CAST(t.receipt AS TEXT),'$.failureCode') IN ('network','permission','quota','cancelled','uncertain','rate_limit','remote') ORDER BY t.rowid LIMIT 1),'')`, signal.RootID).Scan(&code)
	if err != nil {
		return q, err
	}
	if code != "" {
		q.Kind = code
		q.Confidence = "high"
		return q, nil
	}
	if signal.FeedbackRevision == 0 {
		if failed {
			q.Kind = "unknown_failure"
		}
		return q, nil
	}
	f, err := readFeedback(ctx, tx, signal, signal.FeedbackRevision)
	if err != nil {
		return q, err
	}
	if f.SignalStateVersion != signal.StateVersion {
		return q, nil
	}
	q.FeedbackRevision = f.Revision
	if f.Kind == "network" || f.Kind == "quota" || f.Kind == "permission" || f.Kind == "cancelled" {
		q.Kind = f.Kind
		q.Confidence = "reported"
		q.Source = "operator"
		return q, nil
	}
	if f.Kind != "quality_failure" && (f.Kind != "helpful" || failed) {
		return q, nil
	}
	for _, id := range f.ReadIDs {
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id JOIN harness_skill_outcomes o ON o.sequence=l.outcome_seq WHERE l.signal_id=? AND r.id=? AND r.root_id=? AND r.source_key=? AND r.name=? AND r.version=? AND o.run_id=r.run_id AND o.session_id=r.session_id AND o.processed=1 AND o.effects_uncertain=0 AND o.status IN ('completed','failed'))`, signal.ID, id, signal.RootID, signal.Target.SourceIdentity, signal.Target.Name, signal.Version).Scan(&valid); err != nil {
			return q, err
		}
		if !valid {
			return q, errCollectionEvidence
		}
	}
	q.Kind = "skill_quality_success"
	if f.Kind == "quality_failure" {
		q.Kind = "skill_quality_failure"
	}
	q.Confidence, q.Source, q.Eligible = "high", "operator_confirmed", true
	q.EvidenceReadIDs = slices.Clone(f.ReadIDs)
	return q, nil
}
