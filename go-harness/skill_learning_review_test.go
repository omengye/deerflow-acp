package deerflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningReviewManagementRequiresCompleteStrictObjects(t *testing.T) {
	ignore := `{"pattern_id":"p","expected_pattern_revision":1,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":true}`
	if _, err := learningReviewRequest(json.RawMessage(ignore), false); err != nil {
		t.Fatal(err)
	}
	adjudication := map[string]any{"pattern_id": "p", "expected_pattern_revision": 1, "expected_state_version": 0, "scope": "session", "request_key": "decide",
		"target":        map[string]any{"source_id": "project", "source_identity": "identity", "name": "json-check", "scope": "workspace", "workspace": `D:\workspace`},
		"expected_head": map[string]any{"version": "version", "state_version": 2, "exists": true, "enabled": true, "managed": false}, "overlay_base": nil,
		"relation": "body_gap", "reason": "Explicit human decision"}
	data, _ := json.Marshal(adjudication)
	if _, err := learningReviewRequest(data, true); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{"pattern_id":"p","expected_pattern_revision":1,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":null}`,
		`{"pattern_id":"p","expected_pattern_revision":1,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":"true"}`,
		`{"pattern_id":"p","expected_pattern_revision":1,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":true,"ignored":false}`,
		`{"pattern_id":"p","expected_pattern_revision":1.5,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":true}`,
		`{"Pattern_ID":"p","expected_pattern_revision":1,"expected_state_version":0,"scope":"session","request_key":"ignore","ignored":true}`,
	} {
		if _, err := learningReviewRequest(json.RawMessage(invalid), false); err == nil {
			t.Fatal("invalid ignore object accepted", invalid)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { delete(m, "overlay_base") },
		func(m map[string]any) { m["author"] = "model_matched" },
		func(m map[string]any) { delete(m["expected_head"].(map[string]any), "managed") },
		func(m map[string]any) { m["expected_head"].(map[string]any)["managed"] = nil },
		func(m map[string]any) { m["expected_head"].(map[string]any)["Enabled"] = false },
		func(m map[string]any) { m["target"].(map[string]any)["path"] = "other" },
		func(m map[string]any) { m["target"].(map[string]any)["Name"] = "other" },
		func(m map[string]any) { m["overlay_base"] = map[string]any{"name": "json-check"} },
	} {
		var fields map[string]any
		_ = json.Unmarshal(data, &fields)
		mutate(fields)
		invalid, _ := json.Marshal(fields)
		if _, err := learningReviewRequest(invalid, true); err == nil {
			t.Fatal("invalid adjudication object accepted", string(invalid))
		}
	}
}

func TestSkillLearningReviewSDKAndManagementManualControls(t *testing.T) {
	workspace, cfg := skillFixture(t)
	budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 6, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
	cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 3, MaxDailyTokens: 50000, Budget: budget,
		Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
	extractor, consolidator, matcher := &sdkMatchingExtractor{}, &sdkExperienceConsolidator{}, &sdkExperienceMatcher{}
	cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = extractor, consolidator, matcher
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	cfg.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
			event := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolName: "read_file", ToolCallID: "read-full-json", Status: "pending"}
			switch kind {
			case "tool_start":
				event.Arguments = []byte(`{"path":"input.json"}`)
			case "tool_execute":
				event.Status = "in_progress"
			case "tool_end":
				event.Status, event.Content = "completed", []harness.Content{{Type: "text", Text: `{"verified":true}`}}
			}
			if err := emit(ctx, event); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Inspect complete JSON input"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var match harness.SkillExperienceMatch
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		page, err := c.SkillExperienceMatches(ctx, x.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Matches) == 1 {
			match = page.Matches[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if match.Review == nil || match.Target == nil || match.Head == nil {
		t.Fatal("fixture pipeline did not settle", match)
	}
	request := harness.SkillLearningPatternAdjudicationRequest{PatternID: match.PatternID, ExpectedPatternRevision: match.PatternRevision,
		ExpectedStateVersion: match.Review.StateVersion, Scope: "session", RequestKey: "manual-sdk", Target: *match.Target,
		ExpectedHead: *match.Head, OverlayBase: match.OverlayBase, Relation: "already_covered", Reason: "Host explicitly considers the installed method sufficient"}
	result, err := c.AdjudicateSkillLearningPattern(ctx, x.ID, request)
	if err != nil || result.Match == nil || result.Match.Origin != "host_adjudicated" || result.Match.Quality != "unassessed" || result.Review.StateVersion != 1 {
		t.Fatal(result, err)
	}
	replayed, err := skillManagementCall(t, c, map[string]any{"operation": "skill.learning.pattern.adjudicate", "session_id": x.ID, "review": request})
	if err != nil || replayed.(harness.SkillLearningPatternReviewResult).Match.ID != result.Match.ID {
		t.Fatal(replayed, err)
	}
	ignore := harness.SkillLearningPatternIgnoreRequest{PatternID: match.PatternID, ExpectedPatternRevision: match.PatternRevision,
		ExpectedStateVersion: 1, Scope: "session", RequestKey: "manual-ignore", Ignored: true}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.pattern.ignore", "session_id": x.ID, "review": ignore, "source_session_id": x.ID}); err == nil {
		t.Fatal("management widened review authority with extra field")
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.pattern.ignore", "session_id": x.ID, "review": ignore}); err != nil {
		t.Fatal(err)
	}
	result, err = c.SetSkillLearningPatternIgnored(ctx, x.ID, ignore)
	if err != nil || !result.Review.Ignored || result.Review.StateVersion != 2 {
		t.Fatal(result, err)
	}
	var calls, generated int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&calls); err != nil || calls != 3 {
		t.Fatal("manual controls spent paid maintenance calls", calls, err)
	}
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs WHERE owner_kind='skill'`).Scan(&generated); err != nil || generated != 0 || matcher.calls.Load() != 1 {
		t.Fatal("manual review granted generation or model retries", generated, err)
	}
}
