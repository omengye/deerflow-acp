package evolution

import (
	"context"
	"database/sql"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// The cached decision is explanatory only. The worker always re-runs every
// publication guard; a changed policy or proposal makes this hint obsolete.
func (s *Service) autoPatchPolicyIdentity() string {
	var bindings []string
	if s.behavior != nil {
		for key, binding := range s.behavior.suites {
			bindings = append(bindings, key+":"+binding.digest)
		}
	}
	sort.Strings(bindings)
	var options *harness.SkillAutoPatchOptions
	var builtin []string
	if s.autoPatch != nil {
		options = &s.autoPatch.options
		builtin = append(builtin, s.autoPatch.builtin...)
	}
	sort.Strings(builtin)
	var generation any
	if s.generation != nil {
		generation = []any{s.generation.identity, s.generation.reviewIdentity, s.generation.allowed}
	}
	var discovery any
	if s.discovery != nil {
		discovery = s.discovery.options
	}
	var evaluator string
	if s.behavior != nil {
		evaluator = s.behavior.identity
	}
	return encodedHash([]any{"maintenance-policy-v2", options, generation, discovery, bindings, builtin, evaluator})
}

// Correlated against df_skill_proposals p, so both page rows and the total
// count use the same classification, independent of pagination. Never parse
// full candidate packages just to paint the settings badge.
func (s *Service) proposalReasonSQL() (string, []any) {
	fallback := "manual_review"
	if s.autoPatch != nil {
		fallback = "automatic_checks"
	}
	return `CASE WHEN EXISTS(SELECT 1 FROM df_skill_generation_inputs g JOIN df_skill_discoveries d
 ON d.job_id=g.job_id WHERE g.proposal_id=p.id AND json_extract(CAST(d.payload AS TEXT),'$.review_draft')=1)
 THEN 'waiting_evidence' WHEN ?='manual_review' OR json_extract(CAST(p.payload AS TEXT),'$.origin')<>'automatic'
 THEN 'manual_review' WHEN NOT EXISTS(SELECT 1 FROM df_skill_generation_inputs g JOIN df_skill_maintenance_jobs j
 ON j.id=g.job_id WHERE g.proposal_id=p.id AND j.state IN ('queued','running','retry','completed'))
 THEN 'generation_binding_requires_review' ELSE COALESCE((SELECT reason FROM df_skill_auto_patch_decisions a
 WHERE a.proposal_id=p.id AND a.state_version=p.state_version AND a.policy_identity=?),?) END`,
		[]any{fallback, s.autoPatchPolicyIdentity(), fallback}
}

func proposalNeedsAttention(reason string) bool {
	switch reason {
	case "waiting_evidence", "automatic_checks", "waiting_observation", "waiting_quota":
		return false
	default:
		return true
	}
}

func (s *Service) proposalDispositionTx(ctx context.Context, tx *sql.Tx, id string) (*harness.SkillProposalDisposition, error) {
	expression, args := s.proposalReasonSQL()
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT `+expression+` FROM df_skill_proposals p WHERE p.id=?`, append(args, id)...).Scan(&reason)
	return &harness.SkillProposalDisposition{NeedsAttention: proposalNeedsAttention(reason), Reason: reason}, err
}

func (s *Service) saveAutoPatchDecision(ctx context.Context, p Proposal, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO df_skill_auto_patch_decisions(proposal_id,state_version,policy_identity,reason)
 SELECT id,state_version,?,? FROM df_skill_proposals WHERE id=? AND state_version=? AND status='pending_review'
 ON CONFLICT(proposal_id) DO UPDATE SET state_version=excluded.state_version,policy_identity=excluded.policy_identity,reason=excluded.reason
 WHERE state_version<>excluded.state_version OR policy_identity<>excluded.policy_identity OR reason<>excluded.reason`,
		s.autoPatchPolicyIdentity(), reason, p.ID, p.StateVersion)
	return err
}
