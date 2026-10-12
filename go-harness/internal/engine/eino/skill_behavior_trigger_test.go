package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func triggerBehaviorInput(kind string) harness.SkillBehaviorInput {
	count := 0
	if kind == "trigger_positive" {
		count = 1
	}
	return harness.SkillBehaviorInput{EvaluationID: "trigger-eval", CaseID: "trigger", Variant: "candidate", Version: strings.Repeat("b", 64), SkillName: "json-check", Repetition: 1, Files: map[string][]byte{"SKILL.md": []byte("---\nname: json-check\ndescription: Inspect structured JSON\n---\nSECRET_SKILL_BODY\n"), "notes.txt": []byte("SECRET_SUPPORT")}, Case: harness.SkillBehaviorCase{ID: "trigger", Kind: kind, Prompt: "Please inspect structured JSON", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "SECRET_ORACLE", Kind: "skill_selected", Count: count}}, TriggerSkills: []harness.SkillBehaviorTriggerSkill{{Name: "csv-check", Description: "Inspect CSV tables"}}}}
}

func TestSkillTriggerNativeMetadataSelectionAndBudgetReceipts(t *testing.T) {
	s, _, claim, ctx := maintenanceReviewFixture(t, 10)
	var calls atomic.Int64
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []struct{ Role, Content string }
			Tools    []struct {
				Function struct {
					Name       string
					Parameters json.RawMessage
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		data, _ := json.Marshal(request)
		for _, hidden := range []string{"SECRET_ORACLE", "SECRET_SKILL_BODY", "SECRET_SUPPORT", "trigger_positive", "trigger_negative", "skill_selected"} {
			if strings.Contains(string(data), hidden) {
				t.Error("oracle/body leaked to selection model", hidden)
			}
		}
		if len(request.Tools) != 1 || request.Tools[0].Function.Name != "skill" || !strings.Contains(string(request.Tools[0].Function.Parameters), `"skill"`) {
			t.Error("selection does not use the native Skill tool", string(data))
		}
		if !strings.Contains(string(data), "Inspect CSV tables") || !strings.Contains(string(data), "Skills System") {
			t.Error("missing native selection instructions or fixed competitor")
		}
		if strings.Contains(string(data), "Inspect structured JSON") {
			writeBehaviorAnswer(w, "", behaviorCall("select", "skill", `{"skill":"json-check"}`))
		} else {
			writeBehaviorAnswer(w, "No applicable skill")
		}
	}))
	defer server.Close()
	e, err := NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	input := triggerBehaviorInput("trigger_positive")
	input.Case.Prompt = "Check this supplied configuration" // Not the evaluated description.
	candidate, err := e.Evaluate(ctx, input)
	if err != nil || !candidate.Passed || candidate.Selection == nil || candidate.Selection.SelectedName != input.SkillName || candidate.ModelCalls != 1 || candidate.ToolCalls != 0 || candidate.Checks[0].ActualDigest != candidate.OutputDigest {
		t.Fatal(candidate, err)
	}
	input.Variant = "base"
	input.Files["SKILL.md"] = []byte(strings.ReplaceAll(string(input.Files["SKILL.md"]), "Inspect structured JSON", "Handle unrelated legacy logs"))
	base, err := e.Evaluate(ctx, input)
	if err != nil || base.Passed || base.ErrorCode != "assertion_failed" || base.Selection == nil || base.Selection.SelectedName != "" || base.Selection.CatalogDigest == candidate.Selection.CatalogDigest {
		t.Fatal("baseline choice was not separately observed", base, err)
	}
	input.Case = triggerBehaviorInput("trigger_negative").Case
	input.Case.Prompt = "Do the neighboring task"
	negative, err := e.Evaluate(ctx, input)
	if err != nil || !negative.Passed || negative.Selection == nil || negative.Selection.SelectedName != "" {
		t.Fatal(negative, err)
	}
	receipts, err := s.Receipts(ctx, claim.Job.ID, 0, 10)
	if err != nil || len(receipts) != 3 || calls.Load() != 3 {
		t.Fatal(receipts, calls.Load(), err)
	}
	for _, receipt := range receipts {
		if receipt.Stage != "behavior" || receipt.State != "settled" || receipt.Estimated {
			t.Fatal("trigger selection bypassed existing accounting", receipt)
		}
	}
}

func TestSkillTriggerWrongSelectionAndOperationalFailuresRemainDistinct(t *testing.T) {
	for _, mode := range []string{"miss", "overtrigger", "competitor", "invented", "unknown-tool", "duplicate-key", "multiple-tools", "truncated", "empty", "missing-target", "target-override"} {
		t.Run(mode, func(t *testing.T) {
			_, _, _, ctx := maintenanceReviewFixture(t, 4)
			var calls atomic.Int64
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "miss":
					writeBehaviorAnswer(w, "No matching skill")
				case "empty":
					writeBehaviorAnswer(w, "")
				case "competitor":
					writeBehaviorAnswer(w, "", behaviorCall("select", "skill", `{"skill":"csv-check"}`))
				case "invented":
					writeBehaviorAnswer(w, "", behaviorCall("select", "skill", `{"skill":"private-tool"}`))
				case "unknown-tool":
					writeBehaviorAnswer(w, "", behaviorCall("select", "bash", `{"skill":"json-check"}`))
				case "duplicate-key":
					writeBehaviorAnswer(w, "", behaviorCall("select", "skill", `{"skill":"csv-check","skill":"json-check"}`))
				case "multiple-tools":
					writeBehaviorAnswer(w, "", behaviorCall("one", "skill", `{"skill":"json-check"}`), behaviorCall("two", "skill", `{"skill":"csv-check"}`))
				case "truncated":
					_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "No skill"}, "finish_reason": "length"}}})
				default:
					writeBehaviorAnswer(w, "", behaviorCall("select", "skill", `{"skill":"json-check"}`))
				}
			}))
			defer server.Close()
			e, err := NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}})
			if err != nil {
				t.Fatal(err)
			}
			in := triggerBehaviorInput("trigger_positive")
			if mode == "overtrigger" || mode == "competitor" || mode == "unknown-tool" {
				in = triggerBehaviorInput("trigger_negative")
			}
			if mode == "missing-target" {
				in.SkillName = "different-name"
			}
			if mode == "target-override" {
				in.Case.TriggerSkills[0].Name = in.SkillName
			}
			got, err := e.Evaluate(ctx, in)
			want := map[string]string{"miss": "assertion_failed", "overtrigger": "assertion_failed", "competitor": "", "invented": "invalid_tool_arguments", "unknown-tool": "unknown_tool", "duplicate-key": "invalid_tool_arguments", "multiple-tools": "invalid_model_response", "truncated": "invalid_model_response", "empty": "invalid_model_response", "missing-target": "invalid_input", "target-override": "invalid_input"}[mode]
			preflight := mode == "missing-target" || mode == "target-override"
			if got.ErrorCode != want || got.Passed != (mode == "competitor") || (err != nil) != preflight || got.ToolCalls != 0 || calls.Load() != map[bool]int64{false: 1, true: 0}[preflight] {
				t.Fatal(mode, got, err, calls.Load())
			}
			if got.Selection != nil && got.Selection.RuleVersion != skillbehavior.SelectionRuleVersion {
				t.Fatal("unversioned observation")
			}
		})
	}
}

func TestSkillTriggerBudgetCancellationAndUnknownUsageRemainCharged(t *testing.T) {
	for _, mode := range []string{"budget", "cancelled", "unknown-usage", "input-limit"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, parent := maintenanceReviewFixture(t, 1)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			var calls atomic.Int64
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if mode == "cancelled" {
					cancel()
					<-r.Context().Done()
					return
				}
				if mode == "unknown-usage" {
					_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "No applicable skill"}, "finish_reason": "stop"}}})
					return
				}
				writeBehaviorAnswer(w, "No applicable skill")
			}))
			defer server.Close()
			cfg := harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}}
			if mode == "input-limit" {
				cfg.MaxInputBytes = 1024
			}
			e, err := NewSkillBehaviorEvaluator(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			in := triggerBehaviorInput("trigger_negative")
			got, err := e.Evaluate(ctx, in)
			switch mode {
			case "budget":
				if err != nil || !got.Passed {
					t.Fatal(got, err)
				}
				got, err = e.Evaluate(ctx, in)
				if err == nil || got.Passed || got.Selection != nil || got.ErrorCode != "budget_exhausted" {
					t.Fatal("second selection bypassed budget", got, err)
				}
			case "cancelled":
				if err == nil || got.Passed || got.Selection != nil || got.ErrorCode != "cancelled" {
					t.Fatal("cancellation became a negative trigger success", got, err)
				}
			case "unknown-usage":
				if err != nil || !got.Passed || !got.UsageEstimated {
					t.Fatal(got, err)
				}
			case "input-limit":
				if err != nil || got.Passed || got.Selection != nil || got.ErrorCode != "output_limit" {
					t.Fatal("oversized catalog reached provider", got, err)
				}
			}
			receipts, err := s.Receipts(context.WithoutCancel(ctx), claim.Job.ID, 0, 10)
			want := 1
			if mode == "input-limit" {
				want = 0
			}
			if err != nil || len(receipts) != want || calls.Load() != int64(want) {
				t.Fatal("unexpected selection calls/receipts", len(receipts), calls.Load(), err)
			}
			if mode == "cancelled" || mode == "unknown-usage" {
				if !receipts[0].Estimated || receipts[0].ChargedTokens != receipts[0].ReservedTokens {
					t.Fatal("uncertain selection usage refunded", receipts[0])
				}
			}
		})
	}
}
