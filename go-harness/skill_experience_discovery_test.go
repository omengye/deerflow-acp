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

type sdkExperienceReview struct{}

type sdkExperienceMerge struct{}

func (*sdkExperienceMerge) Identity() string { return "sdk-experience-merge-v1" }
func (*sdkExperienceMerge) Consolidate(ctx context.Context, in harness.SkillExperienceConsolidationInput) (harness.SkillExperienceConsolidationDecision, error) {
	if err := sdkMatchingCall(ctx, "experience_consolidate"); err != nil {
		return harness.SkillExperienceConsolidationDecision{}, err
	}
	if len(in.Patterns) != 0 {
		return harness.SkillExperienceConsolidationDecision{Relation: "equivalent", PatternID: in.Patterns[0].ID, Reason: "Equivalent complete input procedure"}, nil
	}
	return harness.SkillExperienceConsolidationDecision{Relation: "distinct", Reason: "No previous equivalent procedure"}, nil
}

func (*sdkExperienceReview) Identity() string { return "sdk-experience-review-v1" }
func (v *sdkExperienceReview) Validate(ctx context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
	for _, stage := range []string{"security", "quality"} {
		if err := sdkMatchingCall(ctx, stage); err != nil {
			return harness.SkillValidationReport{}, err
		}
	}
	return harness.SkillValidationReport{ValidatorVersion: v.Identity(), BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
}

func TestSkillLearningExperienceDiscoverySDKSharedDispatcherLoopbackAndDisabledRecovery(t *testing.T) {
	sdkExperienceDiscoveryLoopback(t, false)
}

func TestSkillLearningExperienceNewSkillSDKSharedDispatcherLoopbackAndDisabledRecovery(t *testing.T) {
	sdkExperienceDiscoveryLoopback(t, true)
}

func TestSkillLearningReviewDraftSDKAutomaticallyGeneratesWithoutManualEpisodeOrRequest(t *testing.T) {
	sdkExperienceDiscoveryLoopbackOptions(t, false, true)
}

type sdkNewCapabilityMatcher struct{}

func (*sdkNewCapabilityMatcher) Identity() string { return "sdk-new-capability-match-v1" }
func (*sdkNewCapabilityMatcher) Match(ctx context.Context, _ harness.SkillExperienceMatchInput) (harness.SkillExperienceMatchDecision, error) {
	if err := sdkMatchingCall(ctx, "skill_match"); err != nil {
		return harness.SkillExperienceMatchDecision{}, err
	}
	return harness.SkillExperienceMatchDecision{Relation: "new_capability", Reason: "A reusable capability not covered by current authorized skills", SuggestedName: "structured-source-audit"}, nil
}

func sdkExperienceDiscoveryLoopback(t *testing.T, newCapability bool) {
	t.Helper()
	sdkExperienceDiscoveryLoopbackOptions(t, newCapability, false)
}

func sdkExperienceDiscoveryLoopbackOptions(t *testing.T, newCapability, reviewDrafts bool) {
	t.Helper()
	workspace, cfg := skillFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Messages []struct{ Role, Content string }
			Tools    []any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "instruction-only historical-experience") {
			t.Error("generation not bounded instruction-only", req, err)
			http.Error(w, "fixture invalid", http.StatusBadRequest)
			return
		}
		var payload struct {
			SkillName, BaseVersion string
			BaseFiles              map[string]struct{ Text string }
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &fields); err != nil {
			t.Error(err)
		}
		_ = json.Unmarshal(fields["skill_name"], &payload.SkillName)
		_ = json.Unmarshal(fields["base_version"], &payload.BaseVersion)
		_ = json.Unmarshal(fields["base_files"], &payload.BaseFiles)
		files := map[string]string{}
		for path, file := range payload.BaseFiles {
			files[path] = file.Text
		}
		files["SKILL.md"] = "---\nname: " + payload.SkillName + "\ndescription: Research structured sources\n---\nRead complete input and verify fields.\n"
		if newCapability {
			if payload.SkillName != "structured-source-audit" || payload.BaseVersion != "" || len(payload.BaseFiles) != 0 || !strings.Contains(req.Messages[0].Content, "Create exactly one file, SKILL.md") {
				t.Error("new capability generation did not fix name/absent base/one-file constraint", payload)
			}
		} else {
			files["SKILL.md"] += "Read guide.txt.\n"
		}
		answer, _ := json.Marshal(map[string]any{"skill_name": payload.SkillName, "base_version": payload.BaseVersion, "reason": "Add complete input verification", "files": files})
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
	}))
	defer server.Close()
	budget := harness.SkillMaintenanceBudget{MaxDailyModelCalls: 20, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}
	cfg.SkillLearning = &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 15, MaxDailyTokens: 50000, Budget: budget, Consolidation: &harness.SkillGenerationModelConfig{}, Matching: &harness.SkillGenerationModelConfig{}}
	cfg.SkillExperienceExtractor, cfg.SkillExperienceConsolidator, cfg.SkillExperienceMatcher = &sdkMatchingExtractor{}, &sdkExperienceMerge{}, &sdkExperienceMatcher{}
	cfg.SkillValidator = &sdkExperienceReview{}
	cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: harness.ModelRoute{Provider: "openai", Model: "fixture", BaseURL: server.URL, APIKey: "unused-fixture"}}, Budget: budget, Sources: []string{"project"}}
	cfg.SkillDiscovery = &harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
	cfg.SkillDiscovery.ExperienceReviewDraftsEnabled = reviewDrafts
	if newCapability {
		cfg.SkillDiscovery.ExperienceNewSkillsEnabled, cfg.SkillDiscovery.NewSkillSourceID = true, "project"
		cfg.SkillExperienceMatcher = &sdkNewCapabilityMatcher{}
	}
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	cfg.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
			event := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolName: "read_file", ToolCallID: "read-input", Status: "pending"}
			if kind == "tool_start" {
				event.Arguments = []byte(`{"path":"input.json"}`)
			} else if kind == "tool_execute" {
				event.Status = "in_progress"
			} else {
				event.Status, event.Content = "completed", []harness.Content{{Type: "text", Text: `{"verified":true}`}}
			}
			if err := emit(ctx, event); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	status, err := c.SkillLearningStatus(ctx)
	if err != nil || status.Mode != "review_candidates" || !status.ProposalDiscoveryEnabled {
		t.Fatal("SDK status still advertises shadow", status, err)
	}
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "First complete structured source"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var matches int
	for time.Now().Before(deadline) {
		_ = c.store.DB().QueryRow(`SELECT count(*) FROM df_learning_matches m JOIN df_skill_maintenance_jobs j ON j.id=m.job_id WHERE j.state='completed'`).Scan(&matches)
		if matches > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if matches != 1 || !reviewDrafts && calls.Load() != 0 {
		t.Fatal("one task generated a proposal or matching did not finish", matches, calls.Load())
	}
	if !reviewDrafts {
		if _, err = c.StartSkillLearningEpisode(ctx, x.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Independent complete structured source"}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	var proposal string
	// Extraction, consolidation, matching and generation share durable workers.
	// Use the existing scenario deadline, not a shorter competing wall clock.
	for ctx.Err() == nil {
		_ = c.store.DB().QueryRowContext(ctx, `SELECT id FROM df_skill_proposals WHERE status='pending_review' LIMIT 1`).Scan(&proposal)
		if proposal != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if proposal == "" || calls.Load() != 1 {
		var codes string
		_ = c.store.DB().QueryRow(`SELECT group_concat(job_kind||':'||state||':'||error_code) FROM df_skill_maintenance_jobs`).Scan(&codes)
		var inputs string
		_ = c.store.DB().QueryRow(`SELECT group_concat(state||':'||reason) FROM df_learning_match_inputs`).Scan(&inputs)
		t.Fatal("SDK did not create one pending review", proposal, calls.Load(), codes, inputs, ctx.Err())
	}
	if reviewDrafts {
		page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
		if err != nil || len(page.Discoveries) != 1 || !page.Discoveries[0].ReviewDraft || len(page.Discoveries[0].Evidence) != 1 {
			t.Fatal("SDK automatic draft did not preserve one-episode manual boundary", page, err)
		}
		managed, err := skillManagementCall(t, c, map[string]any{"operation": "skill.discovery.list", "limit": 10})
		encoded, _ := json.Marshal(managed)
		if err != nil || !strings.Contains(string(encoded), `"review_draft":true`) {
			t.Fatal("MANAGE omitted automatic draft attribution", managed, err)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	// Disabling discovery stops new spending but leaves valid explicit review
	// available when the host keeps the exact learning/evidence adapters.
	cfg.SkillDiscovery = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	status, err = c.SkillLearningStatus(ctx)
	if err != nil || status.Mode != "shadow" || status.ProposalDiscoveryEnabled {
		t.Fatal("disabled discovery still advertised", status, err)
	}
	p, err := c.GetSkillProposal(ctx, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if newCapability && (p.Action != "create" || p.SkillName != "structured-source-audit" || p.Base.Exists || len(p.ChangedFiles) != 1 || p.ChangedFiles[0] != "SKILL.md") {
		t.Fatal("SDK new capability not one-file pending creation", p)
	}
	p, err = c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "fixture-operator"})
	if err != nil || p.Status != "published" || calls.Load() != 1 {
		t.Fatal("disabled-discovery manual recovery failed", p, err, calls.Load())
	}
}
