package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type sdkExperienceExtractor struct {
	calls atomic.Int32
	seen  chan harness.SkillLearningInput
}

func (e *sdkExperienceExtractor) Identity() string { return "sdk-extract-v1" }
func (e *sdkExperienceExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	b, _ := skillmaintenance.BrokerFromContext(ctx)
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: "experience_extract", ModelIdentity: strings.Repeat("a", 64), InputDigest: strings.Repeat("b", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return nil, err
	}
	if err = b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
		return nil, err
	}
	e.calls.Add(1)
	if e.seen != nil {
		e.seen <- input
	}
	return []harness.SkillExperienceCandidate{{Kind: "capability_gap", Goal: "Validate configuration", Applicability: "Project configuration", Resolution: "Read full structured text", Steps: []string{"Read complete input"}, Verification: "Needs verification", Outcome: "unresolved", References: []string{input.References[0].ID}}}, nil
}

func TestSkillLearningSDKNativeExecutionCapturesActualSkillAndTool(t *testing.T) {
	workspace, cfg := skillFixture(t)
	if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"verified":true,"token":"PRIVATE_CONFIG_SECRET"}`), 0600); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool
			Messages []struct{ Role string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		toolResults := 0
		for _, message := range request.Messages {
			if message.Role == "tool" {
				toolResults++
			}
		}
		message := map[string]any{"role": "assistant", "content": "LOCAL_FINAL_VERIFIED"}
		finish := "stop"
		if toolResults < 2 {
			name, args := "skill", `{"skill":"research"}`
			if toolResults == 1 {
				name, args = "read_file", `{"path":"config.json"}`
			}
			message["content"], message["tool_calls"], finish = nil, []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", toolResults), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}, "tool_calls"
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	e := &sdkExperienceExtractor{seen: make(chan harness.SkillLearningInput, 1)}
	cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL = "openai", "fixture", "fixture-only", provider.URL+"/v1"
	cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 1, MaxDailyTokens: 10000, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 3, MaxDailyTokens: 30000, MaxJobSeconds: 30, Workers: 1}}
	cfg.SkillExperienceExtractor = e
	cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 6, MaxToolCalls: 4, MaxTokens: 100000, MaxOutputTokens: 128}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	installed, err := c.InstallSkill(ctx, "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetSkillEnabled(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Use research and inspect config.json"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatal(result, err)
	}
	deadline := time.Now().Add(6 * time.Second)
	var page harness.SkillExperiencePage
	for time.Now().Before(deadline) {
		page, err = c.SkillExperiences(ctx, x.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Experiences) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(page.Experiences) != 1 || e.calls.Load() != 1 {
		var reason, state, jobCode, blocked, reservationStates string
		var inputs, outbox, attempts, reservations int
		_ = c.store.DB().QueryRow(`SELECT count(*) FROM harness_learning_inputs`).Scan(&inputs)
		_ = c.store.DB().QueryRow(`SELECT count(*) FROM harness_learning_outbox`).Scan(&outbox)
		_ = c.store.DB().QueryRow(`SELECT reason,state FROM harness_learning_outbox LIMIT 1`).Scan(&reason, &state)
		_ = c.store.DB().QueryRow(`SELECT error_code FROM df_skill_maintenance_jobs LIMIT 1`).Scan(&jobCode)
		_ = c.store.DB().QueryRow(`SELECT blocked_reason FROM budget_roots LIMIT 1`).Scan(&blocked)
		_ = c.store.DB().QueryRow(`SELECT count(*) FROM budget_attempts WHERE state<>'ended'`).Scan(&attempts)
		_ = c.store.DB().QueryRow(`SELECT count(*) FROM budget_reservations WHERE state<>'settled'`).Scan(&reservations)
		_ = c.store.DB().QueryRow(`SELECT group_concat(state||':'||kind) FROM budget_reservations`).Scan(&reservationStates)
		t.Fatalf("native execution not extracted %+v %d inputs=%d outbox=%d state=%s reason=%s job=%s blocked=%s attempts=%d reservations=%d states=%s", page, e.calls.Load(), inputs, outbox, state, reason, jobCode, blocked, attempts, reservations, reservationStates)
	}
	input := <-e.seen
	var skill, receipt, answer bool
	for _, ref := range input.References {
		if ref.RunID != result.Execution.RunID {
			t.Fatal("foreign source", ref)
		}
		if ref.Kind == "skill_read" {
			skill = ref.Skill != nil && ref.Skill.Version == installed.Ref.Version && ref.Skill.SourceIdentity == installed.Ref.SourceIdentity && ref.Path == "SKILL.md"
		}
		if ref.Kind == "tool_observation" && ref.ToolName == "read_file" {
			receipt = ref.State == "completed" && strings.Contains(ref.Text, "verified") && !ref.Truncated
		}
		if ref.Kind == "assistant" {
			answer = strings.Contains(ref.Text, "LOCAL_FINAL_VERIFIED")
		}
	}
	encoded, _ := json.Marshal(input)
	if !skill || !receipt || !answer || strings.Contains(string(encoded), "PRIVATE_CONFIG_SECRET") {
		t.Fatalf("native source projection incomplete/unsafe skill=%v receipt=%v answer=%v %s", skill, receipt, answer, encoded)
	}
}

func TestSkillLearningSDKShadowLifecycleAndManagement(t *testing.T) {
	ctx := context.Background()
	e := &sdkExperienceExtractor{}
	cfg := Config{DataDir: t.TempDir(), SkillLearning: &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 1, MaxDailyTokens: 10000, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 3, MaxDailyTokens: 30000, MaxJobSeconds: 60, Workers: 1}}, SkillExperienceExtractor: e, Engine: engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "Please validate the complete JSON"}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	x, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Read configuration"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	var page harness.SkillExperiencePage
	for time.Now().Before(deadline) {
		page, err = c.SkillExperiences(ctx, x.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Experiences) != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(page.Experiences) != 1 || e.calls.Load() != 1 {
		t.Fatalf("worker did not persist experience %+v calls=%d", page, e.calls.Load())
	}
	status, err := c.SkillLearningStatus(ctx)
	if err != nil || !status.Enabled || status.Mode != "shadow" || status.Experiences != 1 {
		t.Fatalf("status %+v %v", status, err)
	}
	if generations := c.evolution.GenerationStatus(); generations.Enabled {
		t.Fatal("shadow learning granted skill generation")
	}
	jobs, err := c.SkillLearningJobs(ctx, x.ID, "", 10)
	if err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].State != "completed" {
		t.Fatal(jobs, err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.jobs", "session_id": x.ID, "limit": 10}); err != nil {
		t.Fatal(err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.set", "session_id": x.ID, "control": map[string]any{"contribute": false, "expected_epoch": 1}}); err != nil {
		t.Fatal(err)
	}
	page, err = c.SkillExperiences(ctx, x.ID, "", 10)
	if err != nil || len(page.Experiences) != 0 {
		t.Fatal("opt-out retained derived experience", err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.get", "session_id": x.ID, "operator_confirmed": true}); err == nil {
		t.Fatal("management accepted model authority field")
	}
	for _, raw := range []string{
		`{"contribute":false,"expected_epoch":2,"operator_confirmed":true}`,
		`{"contribute":false,"Contribute":true,"expected_epoch":2}`,
		`{"contribute":false,"contribute":true,"expected_epoch":2}`,
		`{"contribute": null ,"expected_epoch":2}`,
		`{"contribute":false,"expected_epoch": null }`,
		`{"contribute":false,"expected_epoch":2.5}`,
		`{"contribute":false}`,
	} {
		if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.set", "session_id": x.ID, "control": json.RawMessage(raw)}); err == nil {
			t.Fatal("invalid management consent accepted", raw)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SkillLearning, cfg.SkillExperienceExtractor = nil, nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	status, err = c.SkillLearningStatus(ctx)
	if err != nil || status.Enabled {
		t.Fatal("restart without opt-in left learning enabled", err)
	}
}
