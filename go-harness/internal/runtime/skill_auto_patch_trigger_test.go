package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

type autoPatchTriggerEvaluator struct {
	mode  string
	calls int
}

func (*autoPatchTriggerEvaluator) Identity() string { return "learning-trigger-fixture-v1" }
func (e *autoPatchTriggerEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	e.calls++
	if !skillbehavior.TriggerCase(in.Case) {
		mode := "unchanged"
		if e.mode == "body-and-trigger" {
			mode = "on"
		}
		return (&autoPatchLearningEvaluator{mode: mode}).Evaluate(ctx, in)
	}
	if err := consolidationTestCall(ctx, "behavior"); err != nil {
		return harness.SkillBehaviorResult{}, err
	}
	_, digest, err := skillbehavior.SelectionCatalog(in)
	if err != nil {
		return harness.SkillBehaviorResult{}, err
	}
	selected := ""
	if in.Case.Kind == "trigger_positive" && (in.Variant == "candidate" || e.mode == "no-trigger-improvement" || e.mode == "negative-improvement") || in.Case.Kind == "trigger_negative" && (e.mode == "negative-fail" && in.Variant == "candidate" || e.mode == "negative-improvement" && in.Variant == "base") {
		selected = in.SkillName
	}
	if e.mode == "positive-fail" && in.Case.Kind == "trigger_positive" && in.Variant == "candidate" {
		selected = ""
	}
	x := &harness.SkillBehaviorSelection{RuleVersion: skillbehavior.SelectionRuleVersion, TargetName: in.SkillName, SelectedName: selected, CatalogDigest: digest}
	check := skillbehavior.SelectionCheck(in.Case.Assertions[0], *x)
	code := ""
	if !check.Passed {
		code = "assertion_failed"
	}
	out := harness.SkillBehaviorResult{CaseID: in.CaseID, Variant: in.Variant, Version: in.Version, Repetition: in.Repetition, Passed: check.Passed, StopReason: "completed", ErrorCode: code, OutputDigest: check.ActualDigest, TraceDigest: digest, Checks: []harness.SkillBehaviorCheck{check}, ModelCalls: 1, InputTokens: 10, OutputTokens: 5, Selection: x}
	if e.mode == "baseline-timeout" && in.Case.Kind == "trigger_positive" && in.Variant == "base" {
		out.Selection, out.Passed, out.StopReason, out.ErrorCode = nil, false, "failed", "timeout"
	}
	return out, nil
}

func autoPatchTriggerSuite() harness.SkillBehaviorSuite {
	s := autoPatchLearningSuite()
	s.Version = skillbehavior.TriggerVersion
	for _, kind := range []string{"trigger_positive", "trigger_negative"} {
		count := 0
		if kind == "trigger_positive" {
			count = 1
		}
		s.Cases = append(s.Cases, harness.SkillBehaviorCase{ID: kind, Kind: kind, Prompt: "Verify the host-defined applicability boundary", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "expected_selection", Kind: "skill_selected", Count: count}}, TriggerSkills: []harness.SkillBehaviorTriggerSkill{{Name: "csv-check", Description: "Inspect CSV tables, not JSON configuration"}}})
	}
	return s
}

func TestSkillTriggerLearningDescriptionPublicationRequiresIndependentEvidence(t *testing.T) {
	for _, mode := range []string{"description-only", "negative-improvement", "body-and-trigger", "body-without-improvement", "no-trigger-improvement", "positive-fail", "negative-fail", "baseline-timeout", "consent", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			adoptLearningBase(t, r)
			g.descriptionOnly = mode != "body-and-trigger" && mode != "body-without-improvement"
			matcher.relation = "trigger_mismatch"
			evaluator := &autoPatchTriggerEvaluator{mode: mode}
			if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchTriggerSuite()}}); err != nil {
				t.Fatal(err)
			}
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
				t.Fatal(err)
			}
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			p := autoPatchLearningProposal(t, e, ds[0])
			if evaluator.calls != 8 || len(g.seen) != 1 || p.Evaluation.BehaviorReport == nil || p.Evaluation.BehaviorReport.TriggerPositiveCases != 1 || p.Evaluation.BehaviorReport.TriggerNegativeCases != 1 {
				t.Fatal("missing separate bounded trigger pairs", evaluator.calls, p)
			}
			if mode == "consent" {
				control, _ := s.Store.Learning.Control(ctx, x.ID)
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "receipt" {
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1`); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			published := mode == "description-only" || mode == "negative-improvement" || mode == "body-and-trigger"
			got, err := e.Get(ctx, p.ID)
			if mode == "consent" {
				if err == nil {
					t.Fatal("withdrawn description proposal survived", got)
				}
			} else if err != nil || (got.Status == "published") != published {
				t.Fatal("description admission", mode, got, err)
			}
			if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err == nil && (head.Version == p.CandidateVersion) != published {
					t.Fatal("description publication changed an ineligible head", mode, head)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if published {
				view, err := e.BehaviorEvaluation(ctx, p.ID)
				if err != nil || view.Report == nil || view.Report.RuleVersion != "skill-behavior-comparison-v2" || len(view.Results) != 8 {
					t.Fatal("publication lost fixed trigger evidence", view, err)
				}
				for _, result := range view.Results {
					if strings.HasPrefix(result.CaseID, "trigger_") && (result.Selection == nil || result.Selection.TargetName != "json-check" || result.ToolCalls != 0) {
						t.Fatal("trigger observation presented as an actual skill execution", result)
					}
				}
			}
		})
	}
}

func TestSkillTriggerLearningPendingRestartFencesSuiteAndEvaluator(t *testing.T) {
	s, x, r, e, g, v, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, r)
	g.descriptionOnly, matcher.relation = true, "trigger_mismatch"
	evaluator := &autoPatchTriggerEvaluator{}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchTriggerSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	p := autoPatchLearningProposal(t, e, ds[0])
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"competitor-change", "prompt-change", "evaluator-change", "suite-v1", "original"} {
		t.Run(mode, func(t *testing.T) {
			next, err := evolution.New(ctx, r, v)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err := next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			suite := autoPatchTriggerSuite()
			var activeEvaluator harness.SkillBehaviorEvaluator = evaluator
			switch mode {
			case "competitor-change":
				suite.Cases[2].TriggerSkills[0].Description = "Changed competing applicability"
			case "prompt-change":
				suite.Cases[2].Prompt = "Changed trusted case prompt"
			case "evaluator-change":
				activeEvaluator = &autoPatchLearningEvaluator{}
			case "suite-v1":
				suite = autoPatchLearningSuite()
			}
			if err := next.ConfigureBehavior(activeEvaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}); err != nil {
				t.Fatal(err)
			}
			if err := next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			enableExperienceDiscovery(t, next)
			if err := next.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
				t.Fatal(err)
			}
			if err := next.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := next.Get(ctx, p.ID)
			if err != nil || (got.Status == "published") != (mode == "original") || evaluator.calls != 8 || len(g.seen) != 1 {
				t.Fatal("pending description bypassed pinned policy or re-evaluated", mode, got, err, evaluator.calls, len(g.seen))
			}
		})
	}
}

func TestSkillTriggerLearningApprovalReconstructsPersistedCatalogAndLabels(t *testing.T) {
	s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, r)
	g.descriptionOnly, matcher.relation = true, "trigger_mismatch"
	evaluator := &autoPatchTriggerEvaluator{}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchTriggerSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	p := autoPatchLearningProposal(t, e, ds[0])
	view, err := e.BehaviorEvaluation(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var original harness.SkillBehaviorResult
	for _, result := range view.Results {
		if result.CaseID == "trigger_positive" && result.Variant == "candidate" {
			original = result
		}
	}
	if original.Selection == nil || !original.Passed {
		t.Fatal("missing candidate trigger observation", original)
	}
	for _, mode := range []string{"catalog", "decision", "oracle", "restored"} {
		t.Run(mode, func(t *testing.T) {
			value := original
			x := *value.Selection
			value.Selection = &x
			switch mode {
			case "catalog":
				x.CatalogDigest = strings.Repeat("f", 64)
			case "decision":
				x.SelectedName = "csv-check"
			case "oracle":
				x.SelectedName = ""
				value.Checks = []harness.SkillBehaviorCheck{skillbehavior.SelectionCheck(harness.SkillBehaviorAssertion{ID: "expected_selection", Kind: "skill_selected", Count: 0}, x)}
				value.OutputDigest = value.Checks[0].ActualDigest
			}
			data, _ := json.Marshal(value)
			sha := fmt.Sprintf("%x", sha256.Sum256(data))
			if _, err := s.Store.db.Exec(`UPDATE df_skill_behavior_results SET payload=?,payload_sha=? WHERE evaluation_id=? AND case_id=? AND variant='candidate' AND repetition=1`, data, sha, view.ID, original.CaseID); err != nil {
				t.Fatal(err)
			}
			_, queryErr := e.BehaviorEvaluation(ctx, p.ID)
			if (queryErr == nil) != (mode == "restored") {
				t.Fatal("history accepted a rehashed forged catalog/label", mode, queryErr)
			}
			if mode == "restored" {
				if err := e.RunAutoPatch(ctx); err != nil {
					t.Fatal(err)
				}
				got, err := e.Get(ctx, p.ID)
				if err != nil || got.Status != "published" || evaluator.calls != 8 {
					t.Fatal("restored immutable evidence was re-evaluated or not published", got, err, evaluator.calls)
				}
			} else if _, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
				t.Fatal("approval trusted forged trigger result", mode)
			}
		})
	}
}
