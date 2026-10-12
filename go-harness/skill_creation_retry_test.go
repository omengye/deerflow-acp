package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestSkillLearningCreationRetryManagementStrictCompleteObjects(t *testing.T) {
	prepare := map[string]any{"pattern_id": "p", "expected_pattern_revision": 2, "expected_state_version": 0, "scope": "session", "previous_generation_id": "smj_previous"}
	data, _ := json.Marshal(prepare)
	if _, err := learningCreationRetryRequest(data, false); err != nil {
		t.Fatal(err)
	}
	enqueue := map[string]any{}
	for key, value := range prepare {
		enqueue[key] = value
	}
	for key, value := range map[string]any{"request_key": "explicit", "expected_generation_state_version": 3, "previous_proposal_id": "", "expected_proposal_state_version": 0,
		"evidence_reservation_id": "sd_original", "expected_retry_number": 1,
		"target":        map[string]any{"source_id": "project", "source_identity": "identity", "name": "json-audit", "scope": "global"},
		"expected_head": map[string]any{"version": "", "state_version": 0, "exists": false, "enabled": false, "managed": false}, "overlay_base": nil} {
		enqueue[key] = value
	}
	data, _ = json.Marshal(enqueue)
	if _, err := learningCreationRetryRequest(data, true); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { delete(m, "previous_proposal_id") }, func(m map[string]any) { m["expected_generation_state_version"] = 1.5 },
		func(m map[string]any) { m["Request_Key"] = "alias" }, func(m map[string]any) { m["author"] = "model" },
		func(m map[string]any) { m["target"].(map[string]any)["Name"] = "alias" }, func(m map[string]any) { delete(m["expected_head"].(map[string]any), "managed") },
		func(m map[string]any) { m["expected_head"].(map[string]any)["exists"] = nil }, func(m map[string]any) { m["overlay_base"] = map[string]any{} },
	} {
		var fields map[string]any
		_ = json.Unmarshal(data, &fields)
		mutate(fields)
		invalid, _ := json.Marshal(fields)
		if _, err := learningCreationRetryRequest(invalid, true); err == nil {
			t.Fatal("invalid retry accepted", string(invalid))
		}
	}
	duplicate := strings.Replace(string(data), `"request_key":"explicit"`, `"request_key":"explicit","request_key":"another"`, 1)
	if _, err := learningCreationRetryRequest([]byte(duplicate), true); err == nil {
		t.Fatal("duplicate field accepted")
	}
}

func TestSkillLearningCreationRetrySDKManagementSharedDispatcherAndDisabledReplay(t *testing.T) {
	workspace, cfg := skillFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages []struct{ Role, Content string }
			Tools    []any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 || len(req.Tools) != 0 {
			t.Error(req, err)
		}
		if !strings.Contains(req.Messages[0].Content, "instruction-only historical-experience") {
			t.Error("retry not instruction-only")
		}
		answer, _ := json.Marshal(map[string]any{"skill_name": "structured-source-audit", "base_version": "", "reason": "Read complete structured source",
			"files": map[string]string{"SKILL.md": "---\nname: structured-source-audit\ndescription: Inspect structured sources\n---\nRead complete input and verify fields.\n"}})
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130}})
	}))
	defer server.Close()
	budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 25, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
	cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 15, MaxDailyTokens: 50000, Budget: budget, Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
	cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = &sdkMatchingExtractor{}, &sdkExperienceMerge{}, &sdkNewCapabilityMatcher{}
	cfg.SkillValidator = &sdkExperienceReview{}
	cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "unused-fixture"}}, Budget: budget, Sources: []string{"project"}}
	cfg.SkillDiscovery = &harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, ExperienceNewSkillsEnabled: true, NewSkillSourceID: "project", RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
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
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for i, input := range []string{"First complete structured source", "Independent complete structured source"} {
		if i > 0 {
			if _, err = c.StartSkillLearningEpisode(ctx, x.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: input}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	var proposal, job string
	for deadline := time.Now().Add(14 * time.Second); time.Now().Before(deadline); {
		_ = c.store.DB().QueryRow(`SELECT i.proposal_id,i.job_id FROM df_skill_generation_inputs i JOIN df_skill_proposals p ON p.id=i.proposal_id JOIN df_skill_maintenance_jobs j ON j.id=i.job_id WHERE p.status='pending_review' AND j.state='completed' LIMIT 1`).Scan(&proposal, &job)
		if proposal != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if proposal == "" || calls.Load() != 1 {
		t.Fatal("fixture did not settle original creation", proposal, calls.Load())
	}
	p, err := c.GetSkillProposal(ctx, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.RejectSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "fixture"}); err != nil {
		t.Fatal(err)
	}
	page, err := c.SkillExperiencePatterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	pattern := page.Patterns[0]
	prepare := harness.SkillLearningNewSkillRetryPreparationRequest{PatternID: pattern.ID, ExpectedPatternRevision: pattern.Revision, ExpectedStateVersion: pattern.Review.StateVersion, Scope: "session", PreviousGenerationID: job}
	observed, err := c.PrepareSkillLearningNewSkillRetry(ctx, x.ID, prepare)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := skillManagementCall(t, c, map[string]any{"operation": "skill.learning.creation_retry.prepare", "session_id": x.ID, "retry": prepare})
	if err != nil || managed.(harness.SkillLearningNewSkillRetryPreparation) != observed {
		t.Fatal(managed, err)
	}
	request := observed.Request
	request.RequestKey = "sdk-creation-retry"
	if _, err = c.store.DB().Exec(`UPDATE df_skill_discovery_cooldowns SET until_ns=0`); err != nil {
		t.Fatal(err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.creation_retry.enqueue", "session_id": x.ID, "retry": request, "author": "model"}); err == nil {
		t.Fatal("extra field widened retry authority")
	}
	managed, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.creation_retry.enqueue", "session_id": x.ID, "retry": request})
	if err != nil {
		t.Fatal(err)
	}
	result := managed.(harness.SkillLearningNewSkillRetryResult)
	replay, err := c.RetrySkillLearningNewSkill(ctx, x.ID, request)
	if err != nil || replay != result {
		t.Fatal(replay, err)
	}
	var retryProposal string
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		_ = c.store.DB().QueryRow(`SELECT i.proposal_id FROM df_skill_generation_inputs i JOIN df_skill_proposals p ON p.id=i.proposal_id JOIN df_skill_maintenance_jobs j ON j.id=i.job_id WHERE i.job_id=? AND p.status='pending_review' AND j.state='completed'`, result.GenerationID).Scan(&retryProposal)
		if retryProposal != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if retryProposal == "" || calls.Load() != 2 {
		t.Fatal("retry did not settle one manual candidate", retryProposal, calls.Load())
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SkillDiscovery = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	managed, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.creation_retry.enqueue", "session_id": x.ID, "retry": request})
	if err != nil || managed.(harness.SkillLearningNewSkillRetryResult) != result || calls.Load() != 2 {
		t.Fatal("disabled replay reexecuted", managed, err, calls.Load())
	}
	p, err = c.GetSkillProposal(ctx, retryProposal)
	if err != nil {
		t.Fatal(err)
	}
	p, err = c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "fixture-human"})
	if err != nil || p.Status != "published" || calls.Load() != 2 {
		t.Fatal("retry human recovery failed", p, err, calls.Load())
	}
	for query, want := range map[string]int{`SELECT count(*) FROM df_learning_new_skill_consumed`: 1, `SELECT count(*) FROM df_learning_new_skill_retry_spent`: 1, `SELECT count(*) FROM df_skill_discovery_episode_consumed`: 2} {
		var count int
		if err := c.store.DB().QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal(query, count, want, err)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SkillLearning, cfg.SkillGeneration = nil, nil
	cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = nil, nil, nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	replay, err = c.RetrySkillLearningNewSkill(ctx, x.ID, request)
	if err != nil || replay != result || calls.Load() != 2 {
		t.Fatal("disabled host could not retrieve original receipt", replay, err)
	}
	request.RequestKey = "new-disabled-retry"
	if _, err = c.RetrySkillLearningNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("disabled historical receipt granted new work")
	}
}
