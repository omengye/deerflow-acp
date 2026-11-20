package evolution

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) evidenceReadinessTx(ctx context.Context, tx *sql.Tx, p Proposal, b behaviorRecord) (*harness.SkillBehaviorEvidence, error) {
	if s.behavior == nil || s.generation == nil || p.Origin != "automatic" {
		return nil, nil
	}
	view := &harness.SkillBehaviorEvidence{Path: "host_suite", SuiteBound: b.input.Suite != nil,
		HostSuiteAvailable: s.behavior.suites[behaviorKey(p.Target)].digest != "", Missing: []string{}}
	if b.input.Suite != nil {
		if b.input.Derived {
			view.Path = "task_evidence"
		}
		view.SourceTaskBound = b.input.Submission != nil
		err := walkBehaviorCases(ctx, tx, b.input, func(c harness.SkillBehaviorCase) error {
			if c.Kind == "targeted" {
				view.SupportingCases++
			}
			if c.Holdout {
				view.HoldoutCases++
			}
			return nil
		})
		view.NextAction = "The suite is bound. Inspect behavior execution and adoption results; suite readiness alone does not establish a passing verdict."
		return view, err
	}
	if view.HostSuiteAvailable {
		view.Missing = append(view.Missing, "suite_binding_pending")
		view.NextAction = "A matching trusted host suite is available. The host will rebind and resume this candidate; do not resubmit it."
		return view, nil
	}
	view.Path, view.RequiredSupporting, view.RequiredHoldout = "task_evidence", 2, 1
	var job string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&job); err != nil {
		return nil, err
	}
	r, err := readGeneration(ctx, tx, job)
	if err != nil {
		return nil, err
	}
	if r.input.DiscoveryID != "" {
		view.Path = "discovery_evidence"
		view.Missing = append(view.Missing, "independent_task_suite")
		view.NextAction = "The discovery is bound but has no complete trusted task suite. The host requires supporting task oracles and a separate post-generation holdout, then resumes this candidate."
		return view, nil
	}
	o, err := s.submittedOriginTx(ctx, tx, p, r)
	if err == nil && b.input.Submission != nil && (o == nil || *o != *b.input.Submission) {
		err = errDiscoveryEvidence
	}
	if err != nil && !errors.Is(err, harness.ErrPermissionDenied) && !errors.Is(err, errDiscoveryEvidence) {
		return nil, err
	}
	if o == nil || err != nil {
		view.Path = "host_suite_required"
		view.Missing = append(view.Missing, "source_task_binding", "trusted_host_suite")
		view.NextAction = "This candidate has no valid native source-task binding and no matching trusted host suite. A host maintainer must supply independently grounded validation; ordinary waiting or resubmission cannot supply that binding."
		return view, nil
	}
	view.SourceTaskBound = true
	if !p.Base.Exists {
		view.Path = "host_suite_required"
		view.Missing = append(view.Missing, "new_skill_task_matching", "trusted_host_suite")
		view.NextAction = "This is a new skill with no baseline-use evidence. The host needs an independently grounded suite or the discovery task-matching path; do not claim existing-skill evidence applies."
		return view, nil
	}
	counts, err := walkSubmittedOraclesTx(ctx, tx, p, r, o, nil)
	if err != nil {
		return nil, err
	}
	view.SupportingCases, view.HoldoutCases = counts.Supporting, counts.Holdout
	if counts.Supporting < 2 {
		view.Missing = append(view.Missing, "supporting_task_oracles")
	}
	if counts.Holdout < 1 {
		view.Missing = append(view.Missing, "independent_holdout_oracle")
	}
	view.NextAction = "Keep this candidate. Complete independent real tasks in its originating session and workspace using the installed skill and user-provided exact output or artifact acceptance criteria. DeerFlow Desktop collects eligible task evidence automatically. The host needs two supporting task roots and a separate task started after submission. It will bind and resume automatically. Alternatively a maintainer can supply a trusted suite. Model self-reviews and repeated submissions are not evidence."
	return view, nil
}
