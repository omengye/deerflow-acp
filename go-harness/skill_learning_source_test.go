package deerflow

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSkillLearningSourceManagementStrictReadConditions(t *testing.T) {
	valid := `{"kind":"experience","id":"lexp_saved","expected_revision":1,"scope":"session","experience_id":"lexp_saved"}`
	if _, err := learningSourceRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"kind"`, `"Kind"`, 1),
		strings.Replace(valid, `"kind":"experience"`, `"kind":"experience","kind":"pattern"`, 1),
		strings.Replace(valid, `"expected_revision":1`, `"expected_revision":null`, 1),
		strings.Replace(valid, `"expected_revision":1`, `"expected_revision":1.5`, 1),
		strings.Replace(valid, `"expected_revision":1`, `"expected_revision":0`, 1),
		strings.Replace(valid, `"scope":"session",`, "", 1),
		strings.Replace(valid, `"scope":"session"`, `"scope":false`, 1),
		strings.TrimSuffix(valid, "}") + `,"operator_confirmed":true}`,
		"null", "[]",
	} {
		if _, err := learningSourceRequest([]byte(invalid)); err == nil {
			t.Fatal("invalid source conditions accepted", invalid)
		}
	}
}

type sdkSourceExtractor struct{ calls atomic.Int32 }

func (e *sdkSourceExtractor) Identity() string { return "sdk-source-extract-v1" }
func (e *sdkSourceExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	b, ok := skillmaintenance.BrokerFromContext(ctx)
	if !ok {
		return nil, harness.ErrSkillValidation
	}
	r, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: "experience_extract", ModelIdentity: strings.Repeat("a", 64), InputDigest: strings.Repeat("b", 64), InputTokens: 100, MaxOutputTokens: 100})
	if err != nil {
		return nil, err
	}
	if err = b.Settle(ctx, r, skillmaintenance.Usage{InputTokens: 10, OutputTokens: 5, Known: true}); err != nil {
		return nil, err
	}
	e.calls.Add(1)
	refs := make([]string, len(input.References))
	for i, ref := range input.References {
		refs[i] = ref.ID
	}
	return []harness.SkillExperienceCandidate{{Kind: "proven_recipe", Goal: "Read structured input", Applicability: "JSON configuration",
		Resolution: "Read complete input", Steps: []string{"Read full file", "Inspect required fields"}, Verification: "Saved local observation; quality unassessed", Outcome: "observed_execution", References: refs}}, nil
}

func TestSkillLearningSourceSDKManagementCitedHintsDisabledAndWithdrawal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	extractor := &sdkSourceExtractor{}
	cfg := Config{DataDir: t.TempDir(), SkillLearning: &harness.SkillLearningOptions{Idle: time.Second, MaxDailyModelCalls: 1, MaxDailyTokens: 10000,
		Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 3, MaxDailyTokens: 30000, MaxJobSeconds: 30, Workers: 1}}, SkillExperienceExtractor: extractor,
		Engine: engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
			for _, tool := range []string{"read_file", "web_fetch"} {
				for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
					event := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolName: tool, ToolCallID: tool + "-call", Status: "pending"}
					switch kind {
					case "tool_start":
						event.Arguments = []byte(`{"path":"configuration.json","token":"PRIVATE_ARGUMENT_TOKEN"}`)
					case "tool_execute":
						event.Status = "in_progress"
					case "tool_end":
						event.Status = "completed"
						result := `{"verified":true,"token":"PRIVATE_RESULT_TOKEN"}`
						if tool == "web_fetch" {
							result = "PRIVATE_REMOTE_BODY does not enter source inspection"
						}
						event.Content = []harness.Content{{Type: "text", Text: result}}
					}
					if err := emit(ctx, event); err != nil {
						return harness.RunResult{}, err
					}
				}
			}
			if err := emit(ctx, harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "Structured JSON observed; method needs separate evaluation."}); err != nil {
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
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Inspect JSON token=PRIVATE_USER_TOKEN"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var page harness.SkillExperiencePage
	deadline := time.Now().Add(6 * time.Second)
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
	if len(page.Experiences) != 1 {
		t.Fatal("source fixture did not extract", page)
	}
	e := page.Experiences[0]
	request := harness.SkillLearningSourceRequest{Kind: "experience", ID: e.ID, ExpectedRevision: e.Revision, Scope: "session", ExperienceID: e.ID}
	source, err := c.SkillLearningSource(ctx, x.ID, request)
	if err != nil || len(source.References) != 4 || source.Quality != "unassessed" || source.SourceState != "checked_at_read" {
		t.Fatal(source, err)
	}
	encoded, _ := json.Marshal(source)
	for _, secret := range []string{"PRIVATE_USER_TOKEN", "PRIVATE_ARGUMENT_TOKEN", "PRIVATE_RESULT_TOKEN", "PRIVATE_REMOTE_BODY"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private source escaped learning boundary", secret)
		}
	}
	var local, external, answer bool
	for _, ref := range source.References {
		local = local || ref.Kind == "tool_observation" && ref.ToolName == "read_file" && strings.Contains(ref.Text, "verified")
		external = external || ref.Kind == "tool_observation" && ref.ToolName == "web_fetch" && strings.HasPrefix(ref.Text, "Argument shape: ") && !strings.Contains(ref.Text, "Observed result:")
		answer = answer || ref.Kind == "assistant" && strings.Contains(ref.Text, "needs separate evaluation")
	}
	if !local || !external || !answer {
		t.Fatal("source kinds not projected faithfully", source)
	}
	wire := map[string]any{"operation": "skill.learning.source.get", "session_id": x.ID, "source": request}
	managed, err := skillManagementCall(t, c, wire)
	managedJSON, _ := json.Marshal(managed)
	if err != nil || string(managedJSON) != string(encoded) {
		t.Fatal("MANAGE source contract diverged", managed, err)
	}
	wire["operator_confirmed"] = true
	if _, err = skillManagementCall(t, c, wire); err == nil {
		t.Fatal("read accepted unrelated authority field")
	}
	delete(wire, "operator_confirmed")
	foreign, err := c.NewSession(ctx, x.CWD)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SkillLearningSource(ctx, foreign.ID, request); err == nil {
		t.Fatal("SDK granted private source to other session")
	}
	if extractor.calls.Load() != 1 {
		t.Fatal("source inspection made model calls", extractor.calls.Load())
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
	managed, err = skillManagementCall(t, c, wire)
	managedJSON, _ = json.Marshal(managed)
	if err != nil || string(managedJSON) != string(encoded) {
		t.Fatal("disabled paid learning blocked original private inspection", managed, err)
	}
	if _, err = skillManagementCall(t, c, map[string]any{"operation": "skill.learning.control.set", "session_id": x.ID, "control": map[string]any{"contribute": false, "expected_epoch": 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err = skillManagementCall(t, c, wire); err == nil {
		t.Fatal("forgotten source readable through MANAGE")
	}
}
