package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Case results are read in a bounded group (at most two variants times three
// repetitions). Completed evidence can grow without growing worker memory.
func caseResults(ctx context.Context, tx *sql.Tx, r behaviorRecord, c harness.SkillBehaviorCase) ([]harness.SkillBehaviorResult, error) {
	part := r
	suite := *r.input.Suite
	suite.Cases = []harness.SkillBehaviorCase{c}
	part.input.Suite = &suite
	rows, err := tx.QueryContext(ctx, `SELECT payload,payload_sha FROM df_skill_behavior_results WHERE evaluation_id=? AND case_id=? ORDER BY repetition,variant`, r.input.ID, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	expected := map[string]harness.SkillBehaviorInput{}
	for _, in := range behaviorInputs(part) {
		expected[resultKey(in.CaseID, in.Variant, in.Repetition)] = in
	}
	results := make([]harness.SkillBehaviorResult, 0, c.Repetitions*2)
	for rows.Next() {
		var raw []byte
		var sha string
		var value harness.SkillBehaviorResult
		if err = rows.Scan(&raw, &sha); err != nil {
			return nil, err
		}
		if len(raw) > 16<<10 || hash(raw) != sha || json.Unmarshal(raw, &value) != nil {
			return nil, invalid("case result integrity failure")
		}
		in, ok := expected[resultKey(value.CaseID, value.Variant, value.Repetition)]
		if !ok {
			return nil, invalid("unexpected case result")
		}
		if err = validateBehaviorResult(value, in); err != nil {
			return nil, err
		}
		if err = validateTriggerCatalogTx(ctx, tx, r.input.Target, value, in); err != nil {
			return nil, err
		}
		results = append(results, value)
	}
	return results, rows.Err()
}

func storedBehaviorReport(ctx context.Context, tx *sql.Tx, r behaviorRecord) (harness.SkillBehaviorReport, error) {
	var total harness.SkillBehaviorReport
	if r.input.Suite == nil {
		return total, invalid("missing behavior suite")
	}
	err := walkBehaviorCases(ctx, tx, r.input, func(c harness.SkillBehaviorCase) error {
		results, err := caseResults(ctx, tx, r, c)
		if err != nil {
			return err
		}
		part := r
		suite := *r.input.Suite
		suite.Cases = []harness.SkillBehaviorCase{c}
		part.input.Suite = &suite
		report, err := composeBehaviorReport(part, results)
		if err != nil {
			return err
		}
		if total.EvaluationID == "" {
			total = report
			return nil
		}
		total.HoldoutCases += report.HoldoutCases
		total.CandidateFailures += report.CandidateFailures
		total.MeasurementErrors += report.MeasurementErrors
		total.TotalComparisons += report.TotalComparisons
		total.TargetedCases += report.TargetedCases
		total.RegressionCases += report.RegressionCases
		total.TriggerPositiveCases += report.TriggerPositiveCases
		total.TriggerNegativeCases += report.TriggerNegativeCases
		total.Regressions += report.Regressions
		total.Improvements += report.Improvements
		total.TargetedImprovements += report.TargetedImprovements
		total.ModelCalls += report.ModelCalls
		total.ToolCalls += report.ToolCalls
		total.InputTokens += report.InputTokens
		total.OutputTokens += report.OutputTokens
		total.ElapsedMillis += report.ElapsedMillis
		total.UsageEstimated = total.UsageEstimated || report.UsageEstimated
		if len(total.Results) < 32 {
			total.Results = append(total.Results, report.Results[:min(32-len(total.Results), len(report.Results))]...)
		}
		return nil
	})
	if err != nil {
		return total, err
	}
	total.State = "passed"
	if total.TargetedCases == 0 || total.CandidateFailures > 0 || total.MeasurementErrors > 0 || total.Regressions > 0 {
		total.State = "failed"
	}
	return total, nil
}

func storedScriptsCovered(ctx context.Context, tx *sql.Tx, r behaviorRecord, files map[string][]byte) (bool, error) {
	all := []harness.SkillBehaviorResult{}
	if r.input.Suite == nil {
		return false, nil
	}
	seen := map[string]bool{}
	err := walkBehaviorCases(ctx, tx, r.input, func(c harness.SkillBehaviorCase) error {
		values, err := caseResults(ctx, tx, r, c)
		if err != nil {
			return err
		}
		for _, v := range values {
			if v.Variant != "candidate" {
				continue
			}
			for _, execution := range v.Scripts {
				if execution.ExitCode != 0 {
					return invalid("script execution failed")
				}
				if seen[execution.Path] {
					continue
				}
				seen[execution.Path] = true
				all = append(all, harness.SkillBehaviorResult{Variant: "candidate", Scripts: []harness.SkillScriptExecution{execution}})
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return scriptsCovered(files, all), nil
}

func behaviorResultPage(ctx context.Context, tx *sql.Tx, r behaviorRecord, after int64, limit int) ([]harness.SkillBehaviorResult, int64, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid,case_id FROM df_skill_behavior_results WHERE evaluation_id=? AND rowid>? ORDER BY rowid LIMIT ?`, r.input.ID, after, limit+1)
	if err != nil {
		return nil, after, false, err
	}
	type row struct {
		seq int64
		id  string
	}
	var keys []row
	for rows.Next() {
		var k row
		if err = rows.Scan(&k.seq, &k.id); err != nil {
			break
		}
		keys = append(keys, k)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, after, false, err
	}
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	result := make([]harness.SkillBehaviorResult, 0, len(keys))
	for _, k := range keys {
		var raw []byte
		var sha string
		var v harness.SkillBehaviorResult
		if err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_behavior_results WHERE rowid=? AND evaluation_id=?`, k.seq, r.input.ID).Scan(&raw, &sha); err != nil {
			return nil, after, false, err
		}
		if len(raw) > 16<<10 || hash(raw) != sha || json.Unmarshal(raw, &v) != nil {
			return nil, after, false, invalid("result page integrity failure")
		}
		fixed, err := findBehaviorCase(ctx, tx, r.input, k.id)
		if err != nil {
			return nil, after, false, err
		}
		version := r.input.Base.Version
		if v.Variant == "candidate" {
			version = r.input.CandidateVersion
		}
		in := harness.SkillBehaviorInput{CaseID: fixed.ID, Variant: v.Variant, Version: version, SkillName: r.input.Target.Name, Repetition: v.Repetition, Case: *fixed}
		if err = validateBehaviorResult(v, in); err != nil {
			return nil, after, false, err
		}
		if err = validateTriggerCatalogTx(ctx, tx, r.input.Target, v, in); err != nil {
			return nil, after, false, err
		}
		result = append(result, v)
		after = k.seq
	}
	return result, after, more, nil
}
