package evolution

import (
	"context"
	"slices"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) autoPatchReadiness(ctx context.Context) (*harness.SkillAutoPatchReadiness, error) {
	r := &harness.SkillAutoPatchReadiness{State: "disabled", NewSkillsRequireReview: true, Reasons: []string{}}
	allowedSources, where, args, _, err := s.querySources(harness.SkillQuery{Limit: 1}, "maintenance-readiness")
	if err != nil {
		return nil, err
	}
	builtin, err := s.registry.BuiltinSources()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&r.CatalogVersion); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_proposals WHERE `+where+` AND status='pending_review'`, args...).Scan(&r.PendingReview); err != nil {
		return nil, err
	}
	expression, parameters := s.proposalReasonSQL()
	counts, err := tx.QueryContext(ctx, `SELECT reason,count(*) FROM (SELECT `+expression+` AS reason FROM df_skill_proposals p WHERE `+where+` AND status='pending_review') GROUP BY reason`, append(parameters, args...)...)
	if err != nil {
		return nil, err
	}
	for counts.Next() {
		var reason string
		var count int64
		if err = counts.Scan(&reason, &count); err != nil {
			break
		}
		if proposalNeedsAttention(reason) {
			r.NeedsAttention += count
		} else {
			r.WaitingCandidates += count
		}
	}
	if err == nil {
		err = counts.Err()
	}
	counts.Close()
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.source_key,i.name,i.enabled,COALESCE(c.managed,0),
 EXISTS(SELECT 1 FROM df_skill_overlays o WHERE o.source_key=i.source_key AND o.name=i.name)
 FROM df_skill_installs i LEFT JOIN df_skill_control c ON c.source_key=i.source_key AND c.name=i.name
 WHERE `+strings.ReplaceAll(where, "source_key", "i.source_key"), args...)
	if err != nil {
		return nil, err
	}
	type target struct {
		source, name              string
		enabled, managed, overlay bool
	}
	var targets []target
	for rows.Next() {
		var item target
		if err = rows.Scan(&item.source, &item.name, &item.enabled, &item.managed, &item.overlay); err != nil {
			break
		}
		targets = append(targets, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	r.Installed = int64(len(targets))
	for _, item := range targets {
		if !item.enabled || s.generation == nil || slices.Contains(builtin, allowedSources[item.source].SourceID) {
			continue
		}
		var allowed bool
		for _, source := range s.generation.allowed {
			if source.SourceIdentity == item.source {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		if !item.managed || item.overlay {
			continue
		}
		covered := false
		if s.behavior != nil {
			binding, ok := s.behavior.suites[item.source+"/"+item.name]
			covered = ok && suiteCoversAutomaticBody(binding.suite)
		}
		if covered {
			r.EligibleTargets++
		} else {
			r.MissingSuite++
		}
	}
	var options *harness.SkillAutoPatchOptions
	if s.autoPatch != nil {
		options = &s.autoPatch.options
		r.State = "partial"
		if r.EligibleTargets > 0 && r.EligibleTargets == r.Installed {
			r.State = "ready"
		}
	}
	r.PolicyIdentity = s.autoPatchPolicyIdentity()
	if options == nil {
		r.Reasons = append(r.Reasons, "automatic_publication_disabled")
	}
	if r.MissingSuite > 0 {
		r.Reasons = append(r.Reasons, "trusted_suite_missing")
	}
	if r.EligibleTargets < r.Installed {
		r.Reasons = append(r.Reasons, "some_targets_require_review")
	}
	if r.EligibleTargets == 0 {
		r.Reasons = append(r.Reasons, "no_eligible_targets")
	}
	return r, tx.Commit()
}

func suiteCoversAutomaticBody(s harness.SkillBehaviorSuite) bool {
	targeted, regression := false, false
	for _, c := range s.Cases {
		targeted = targeted || c.Kind == "targeted"
		regression = regression || c.Kind == "regression"
	}
	return targeted && regression
}
