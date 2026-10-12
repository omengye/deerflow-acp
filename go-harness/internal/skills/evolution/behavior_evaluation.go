package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const behaviorRuleVersion = "skill-behavior-comparison-v1"
const behaviorTriggerRuleVersion = "skill-behavior-comparison-v2"

// Every stage uses the proposal's publication transaction and the maintenance
// claim, including cache reads. A completed case never grants another worker
// authority to commit against a different source, head, or suite.
func (s *Service) withBehaviorWork(ctx context.Context, input ValidationInput, fn func(*sql.Tx, behaviorRecord) error) error {
	return s.withProposal(ctx, input.Proposal.ID, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.StateVersion != input.Proposal.StateVersion || p.Status != "validating" {
			return harness.ErrSkillConflict
		}
		if err := s.guardGenerationValidation(ctx, tx, *p); err != nil {
			return err
		}
		r, err := readBehavior(ctx, tx.SQL(), p.ID)
		if err != nil {
			return err
		}
		if err = s.checkBehavior(ctx, tx.SQL(), r, *p); err != nil {
			return err
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if !skills.SameOverlayBase(overlay, r.input.OverlayBase) {
			return harness.ErrSkillConflict
		}
		return fn(tx.SQL(), r)
	})
}

func (s *Service) behaviorContent(ctx context.Context, input ValidationInput) (*Report, bool, error) {
	_, err := readBehavior(ctx, s.db, input.Proposal.ID)
	if errors.Is(err, harness.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var report *Report
	err = s.withBehaviorWork(ctx, input, func(_ *sql.Tx, r behaviorRecord) error {
		if r.content != nil {
			if !reportBoundTo(*r.content, input) || r.content.ValidatorVersion != r.input.ValidatorIdentity || r.content.Behavior != "not_run" || r.content.BehaviorReport != nil || r.content.BehaviorDigest != "" || r.content.IsolatedScripts {
				return invalid("cached content review is invalid")
			}
			report = r.content
		}
		return nil
	})
	return report, true, err
}

func (s *Service) saveBehaviorContent(ctx context.Context, input ValidationInput, report Report) error {
	if !reportBoundTo(report, input) {
		return invalid("content review is not bound to candidate")
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return s.withBehaviorWork(ctx, input, func(tx *sql.Tx, r behaviorRecord) error {
		if report.ValidatorVersion != r.input.ValidatorIdentity || report.Behavior != "not_run" || report.BehaviorReport != nil || report.BehaviorDigest != "" || report.IsolatedScripts {
			return invalid("content reviewer returned invalid provenance")
		}
		if r.content != nil {
			if encodedHash(*r.content) != hash(data) {
				return harness.ErrSkillConflict
			}
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE df_skill_behavior_evaluations SET content_report=?,content_sha=? WHERE id=? AND content_report IS NULL`, data, hash(data), r.input.ID)
		return err
	})
}

func resultKey(caseID, variant string, repetition int) string {
	return fmt.Sprintf("%s/%s/%d", caseID, variant, repetition)
}

func behaviorErrorCode(code string) bool {
	switch code {
	case "", "invalid_input", "workspace_error", "model_error", "timeout", "cancelled", "budget_exhausted", "unknown_tool", "duplicate_tool_call", "invalid_tool_arguments", "tool_limit", "model_limit", "fixture_mismatch", "tool_error", "invalid_model_response", "output_limit", "assertion_failed":
		return true
	default:
		return false
	}
}

func validateBehaviorResult(r harness.SkillBehaviorResult, in harness.SkillBehaviorInput) error {
	if r.CaseID != in.CaseID || r.Variant != in.Variant || r.Version != in.Version || r.Repetition != in.Repetition || !hexDigest(r.OutputDigest) || !hexDigest(r.TraceDigest) || !behaviorErrorCode(r.ErrorCode) || r.ModelCalls < 0 || r.ModelCalls > 1000 || r.ToolCalls < 0 || r.ToolCalls > 1000 || r.InputTokens < 0 || r.InputTokens > 1000000000 || r.OutputTokens < 0 || r.OutputTokens > 1000000000 || r.ElapsedMillis < 0 || r.ElapsedMillis > 3600000 || len(r.Checks) != len(in.Case.Assertions) {
		return invalid("behavior result identity or bounds are invalid")
	}
	if r.StopReason != "completed" && r.StopReason != "failed" || r.Passed && (r.StopReason != "completed" || r.ErrorCode != "") || !r.Passed && r.ErrorCode == "" {
		return invalid("behavior result outcome is inconsistent")
	}
	want := map[string]bool{}
	for _, a := range in.Case.Assertions {
		want[a.ID] = true
	}
	for _, c := range r.Checks {
		if !want[c.ID] || !hexDigest(c.ActualDigest) || r.Passed && !c.Passed {
			return invalid("behavior result checks are inconsistent")
		}
		delete(want, c.ID)
	}
	if len(want) != 0 {
		return invalid("behavior result checks are incomplete")
	}
	if err := validateTriggerResult(r, in); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return invalid("behavior result exceeds bounds")
	}
	return nil
}

func behaviorInputs(r behaviorRecord) []harness.SkillBehaviorInput {
	result := []harness.SkillBehaviorInput{}
	if r.input.Suite == nil {
		return result
	}
	for _, c := range r.input.Suite.Cases {
		for rep := 1; rep <= c.Repetitions; rep++ {
			for _, variant := range []string{"base", "candidate"} {
				version := r.input.Base.Version
				if variant == "candidate" {
					version = r.input.CandidateVersion
				}
				result = append(result, harness.SkillBehaviorInput{EvaluationID: r.input.ID, CaseID: c.ID, Variant: variant, Version: version, SkillName: r.input.Target.Name, Repetition: rep, Case: c})
			}
		}
	}
	return result
}

func readBehaviorResults(ctx context.Context, tx *sql.Tx, r behaviorRecord) ([]harness.SkillBehaviorResult, error) {
	expected := map[string]harness.SkillBehaviorInput{}
	for _, in := range behaviorInputs(r) {
		expected[resultKey(in.CaseID, in.Variant, in.Repetition)] = in
	}
	rows, err := tx.QueryContext(ctx, `SELECT case_id,variant,repetition,payload,payload_sha FROM df_skill_behavior_results WHERE evaluation_id=? ORDER BY case_id,repetition,variant LIMIT 97`, r.input.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []harness.SkillBehaviorResult{}
	for rows.Next() {
		var caseID, variant, digest string
		var rep int
		var data []byte
		if err = rows.Scan(&caseID, &variant, &rep, &data, &digest); err != nil {
			return nil, err
		}
		in, ok := expected[resultKey(caseID, variant, rep)]
		var value harness.SkillBehaviorResult
		if !ok || len(result) >= 96 || len(data) > 16<<10 || hash(data) != digest || json.Unmarshal(data, &value) != nil {
			return nil, invalid("stored behavior result is inconsistent")
		}
		if err = validateBehaviorResult(value, in); err != nil {
			return nil, err
		}
		if err = validateTriggerCatalogTx(ctx, tx, r.input.Target, value, in); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func composeBehaviorReport(r behaviorRecord, results []harness.SkillBehaviorResult) (harness.SkillBehaviorReport, error) {
	var report harness.SkillBehaviorReport
	if r.input.Suite == nil {
		return report, invalid("behavior report has no applicable suite")
	}
	byKey := map[string]harness.SkillBehaviorResult{}
	for _, v := range results {
		key := resultKey(v.CaseID, v.Variant, v.Repetition)
		if _, exists := byKey[key]; exists {
			return report, invalid("duplicate behavior result")
		}
		byKey[key] = v
	}
	inputs := behaviorInputs(r)
	if len(results) != len(inputs) {
		return report, invalid("behavior report requires complete pairs")
	}
	for _, in := range inputs {
		value, ok := byKey[resultKey(in.CaseID, in.Variant, in.Repetition)]
		if !ok {
			return report, invalid("behavior result is missing")
		}
		if err := validateBehaviorResult(value, in); err != nil {
			return report, err
		}
	}
	report = harness.SkillBehaviorReport{EvaluationID: r.input.ID, EvaluatorIdentity: r.input.EvaluatorIdentity, SuiteID: r.input.Suite.ID, SuiteDigest: r.input.SuiteDigest, RuleVersion: behaviorRuleVersion, BaseVersion: r.input.Base.Version, CandidateVersion: r.input.CandidateVersion, State: "passed", Results: []harness.SkillBehaviorComparison{}}
	if r.input.Suite.Version == skillbehavior.TriggerVersion {
		report.RuleVersion = behaviorTriggerRuleVersion
	}
	for _, c := range r.input.Suite.Cases {
		switch c.Kind {
		case "targeted":
			report.TargetedCases++
		case "regression":
			report.RegressionCases++
		case "trigger_positive":
			report.TriggerPositiveCases++
		case "trigger_negative":
			report.TriggerNegativeCases++
		}
		for rep := 1; rep <= c.Repetitions; rep++ {
			base, candidate := byKey[resultKey(c.ID, "base", rep)], byKey[resultKey(c.ID, "candidate", rep)]
			report.Results = append(report.Results, harness.SkillBehaviorComparison{CaseID: c.ID, Kind: c.Kind, Repetition: rep, BasePassed: base.Passed, CandidatePassed: candidate.Passed, BaseOutputDigest: base.OutputDigest, CandidateOutputDigest: candidate.OutputDigest, BaseErrorCode: base.ErrorCode, CandidateErrorCode: candidate.ErrorCode, BaseSelection: base.Selection, CandidateSelection: candidate.Selection})
			if base.Passed && !candidate.Passed {
				report.Regressions++
			}
			if !base.Passed && candidate.Passed {
				report.Improvements++
			}
			if (c.Kind == "targeted" || skillbehavior.TriggerCase(c)) && !candidate.Passed {
				report.State = "failed"
			}
			for _, v := range []harness.SkillBehaviorResult{base, candidate} {
				report.ModelCalls += v.ModelCalls
				report.ToolCalls += v.ToolCalls
				report.InputTokens += v.InputTokens
				report.OutputTokens += v.OutputTokens
				report.ElapsedMillis += v.ElapsedMillis
				report.UsageEstimated = report.UsageEstimated || v.UsageEstimated
			}
		}
	}
	if report.TargetedCases == 0 || report.Regressions > 0 {
		report.State = "failed"
	}
	data, err := json.Marshal(report)
	if err != nil || len(data) > 48<<10 {
		return report, invalid("behavior report exceeds bounds")
	}
	return report, nil
}

func (s *Service) runBehavior(ctx context.Context, input ValidationInput, report Report) (Report, error) {
	var record behaviorRecord
	var results []harness.SkillBehaviorResult
	err := s.withBehaviorWork(ctx, input, func(tx *sql.Tx, r behaviorRecord) error {
		record = r
		var err error
		results, err = readBehaviorResults(ctx, tx, r)
		return err
	})
	if err != nil {
		report.Behavior = "unavailable"
		return report, err
	}
	if record.input.Suite == nil {
		note := "Behavior not run: no trusted host suite matches this skill; manual review is required."
		if len(report.Evidence) < 64 {
			report.Evidence = append(report.Evidence, note)
		}
		return report, nil
	}
	if _, ok := skillmaintenance.BrokerFromContext(ctx); !ok {
		report.Behavior = "unavailable"
		return report, harness.ErrPermissionDenied
	}
	completed := map[string]bool{}
	for _, r := range results {
		completed[resultKey(r.CaseID, r.Variant, r.Repetition)] = true
	}
	for _, in := range behaviorInputs(record) {
		if completed[resultKey(in.CaseID, in.Variant, in.Repetition)] {
			continue
		}
		if err = s.withBehaviorWork(ctx, input, func(_ *sql.Tx, _ behaviorRecord) error { return nil }); err != nil {
			break
		}
		// Detach fixtures and files before invoking an SDK extension.
		evalInput := in
		encoded, _ := json.Marshal(in.Case)
		var fixedCase harness.SkillBehaviorCase
		_ = json.Unmarshal(encoded, &fixedCase)
		evalInput.Case = fixedCase
		evalInput.Files = map[string][]byte{}
		files := input.BaseFiles
		if in.Variant == "candidate" {
			files = input.CandidateFiles
		}
		for k, v := range files {
			evalInput.Files[k] = append([]byte(nil), v...)
		}
		var result harness.SkillBehaviorResult
		result, err = s.behavior.evaluator.Evaluate(ctx, evalInput)
		if err != nil {
			break
		}
		if err = validateBehaviorResult(result, in); err != nil {
			break
		}
		catalogInput := in
		catalogInput.Files = files
		if err = validateTriggerCatalog(result, catalogInput); err != nil {
			break
		}
		data, _ := json.Marshal(result)
		err = s.withBehaviorWork(ctx, input, func(tx *sql.Tx, r behaviorRecord) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO df_skill_behavior_results(evaluation_id,case_id,variant,repetition,payload,payload_sha) VALUES(?,?,?,?,?,?)`, r.input.ID, in.CaseID, in.Variant, in.Repetition, data, hash(data))
			return err
		})
		if err != nil {
			break
		}
	}
	if err != nil {
		report.Behavior = "unavailable"
		return report, err
	}
	var native harness.SkillBehaviorReport
	err = s.withBehaviorWork(ctx, input, func(tx *sql.Tx, r behaviorRecord) error {
		values, err := readBehaviorResults(ctx, tx, r)
		if err != nil {
			return err
		}
		native, err = composeBehaviorReport(r, values)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(native)
		if r.report != nil {
			if encodedHash(*r.report) != hash(data) {
				return invalid("saved behavior report does not match cases")
			}
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE df_skill_behavior_evaluations SET report=?,report_sha=? WHERE id=? AND report IS NULL`, data, hash(data), r.input.ID)
		return err
	})
	if err != nil {
		report.Behavior = "unavailable"
		return report, err
	}
	report.Behavior = native.State
	report.BehaviorReport = &native
	report.BehaviorDigest = encodedHash(native)
	if !reportBoundTo(report, input) {
		report.Behavior = "unavailable"
		report.BehaviorReport = nil
		report.BehaviorDigest = ""
		return report, invalid("combined validation report exceeds bounds")
	}
	return report, nil
}

// Approval uses persisted evidence without requiring today's evaluator to be
// enabled. Publication still checks source, complete head and discovery facts.
func (s *Service) guardBehaviorReport(ctx context.Context, tx *sql.Tx, p Proposal, report Report) error {
	r, err := readBehavior(ctx, tx, p.ID)
	if errors.Is(err, harness.ErrNotFound) {
		if report.BehaviorReport != nil || report.BehaviorDigest != "" {
			return ErrValidation
		}
		return nil
	}
	if err != nil {
		return err
	}
	if r.input.Target != p.Target || r.input.Base != p.Base || r.input.CandidateVersion != p.CandidateVersion || !skills.SameOverlayBase(r.input.OverlayBase, p.OverlayBase) || report.ValidatorVersion != r.input.ValidatorIdentity || report.IsolatedScripts {
		return ErrValidation
	}
	if report.BehaviorReport == nil {
		if report.BehaviorDigest != "" || report.Behavior == "passed" {
			return ErrValidation
		}
		if r.input.Suite != nil && report.Security == "passed" {
			passed := true
			for _, c := range report.ModelChecks {
				passed = passed && c.Passed
			}
			if passed && report.Behavior == "not_run" {
				return ErrValidation
			}
		}
		return nil
	}
	if r.report == nil || r.content == nil || report.BehaviorDigest != encodedHash(*report.BehaviorReport) || report.Behavior != report.BehaviorReport.State {
		return ErrValidation
	}
	values, err := readBehaviorResults(ctx, tx, r)
	if err != nil {
		return err
	}
	native, err := composeBehaviorReport(r, values)
	if err != nil {
		return err
	}
	if encodedHash(native) != report.BehaviorDigest || encodedHash(*r.report) != report.BehaviorDigest {
		return ErrValidation
	}
	content := report
	content.Behavior = "not_run"
	content.BehaviorReport = nil
	content.BehaviorDigest = ""
	if encodedHash(content) != encodedHash(*r.content) {
		return ErrValidation
	}
	return nil
}
