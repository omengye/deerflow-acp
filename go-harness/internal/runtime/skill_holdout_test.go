package runtime

import (
	"context"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"testing"
)

type holdoutEvaluator struct{ autoPatchLearningEvaluator }

func (e *holdoutEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	out, err := e.autoPatchLearningEvaluator.Evaluate(ctx, in)
	if err != nil || !skillbehavior.TriggerCase(in.Case) {
		return out, err
	}
	_, digest, err := skillbehavior.SelectionCatalog(in)
	if err != nil {
		return out, err
	}
	selected := ""
	if in.Case.Kind == "trigger_positive" && in.Version != "" {
		selected = in.SkillName
	}
	selection := harness.SkillBehaviorSelection{RuleVersion: skillbehavior.SelectionRuleVersion, TargetName: in.SkillName, SelectedName: selected, CatalogDigest: digest}
	check := skillbehavior.SelectionCheck(in.Case.Assertions[0], selection)
	out.Selection = &selection
	out.Checks = []harness.SkillBehaviorCheck{check}
	out.Passed = check.Passed
	out.OutputDigest = check.ActualDigest
	out.ErrorCode = ""
	if !out.Passed {
		out.ErrorCode = "assertion_failed"
	}
	return out, nil
}
func TestNewHoldoutUnblocksOriginalCandidateWithoutGenerationReplay(t *testing.T) {
	ctx := context.Background()
	s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
	adoptLearningBase(t, r)
	g.preserveFrontmatter = true
	s.Store.CaptureSkillOracle = e.CaptureTaskOracleTx
	eval := &holdoutEvaluator{}
	if err := e.ConfigureBehavior(eval, harness.SkillBehaviorOptions{}); err != nil {
		t.Fatal(err)
	}
	task := func(i int) {
		req := learningTestRun(t, s, x, fmt.Sprintf("Verify complete JSON document %d.\nExpected output:\n```text\nverified\n```", i))
		for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
			event := receiptEvent(req, kind, "read-json", "read_file")
			if kind == "tool_start" {
				event.Arguments = []byte(`{"path":"config.json"}`)
			}
			if kind == "tool_end" {
				event.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
			}
			if _, err := s.Store.Append(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "verified"}); err != nil {
			t.Fatal(err)
		}
		learningTestFinish(t, s, req, "end_turn")
		learningTestQueue(t, s)
		if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
			t.Fatal("extract holdout", worked, err)
		}
		consolidateRun(t, s)
	}
	task(1)
	task(2)
	experienceMatch(t, s)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 {
		t.Fatal("initial evidence not discovered", len(discoveries))
	}
	first := autoPatchLearningProposal(t, e, discoveries[0])
	job, err := e.GetGeneration(ctx, discoveries[0].GenerationID)
	if err != nil || job.State != "waiting" || job.ErrorCode != "blocked_evidence" || len(g.seen) != 1 {
		t.Fatal("candidate must await post-generation holdout", job.State, job.ErrorCode, err)
	}
	task(3)
	// Restart also verifies that new source evidence does not reset generation or
	// invalidate the candidate's pinned original pattern revision.
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err = e.ConfigureGeneration(ctx, g, consolidationTestOptions().Policy, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	enableExperienceDiscovery(t, e)
	if err = e.ConfigureBehavior(eval, harness.SkillBehaviorOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	var done harness.SkillProposal
	for range 12 {
		if _, err = e.RunGeneration(ctx); err != nil {
			t.Fatal(err)
		}
		if err = e.RunAutoPatch(ctx); err != nil {
			t.Fatal(err)
		}
		done, err = e.Get(ctx, first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if done.Status == "published" {
			break
		}
	}
	if done.Status != "published" || done.CandidateVersion != first.CandidateVersion || len(g.seen) != 1 {
		t.Fatal("holdout did not resume original candidate", done.Status, done.Error, len(g.seen))
	}
	var cases int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_behavior_cases WHERE suite_digest=(SELECT json_extract(CAST(payload AS TEXT),'$.suite_digest') FROM df_skill_behavior_evaluations WHERE proposal_id=?)`, done.ID).Scan(&cases); err != nil || cases < 5 {
		t.Fatal("missing host cases and routing comparisons", cases, err)
	}
}
