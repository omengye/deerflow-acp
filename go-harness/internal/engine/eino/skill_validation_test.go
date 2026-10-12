package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func reviewInput() harness.SkillValidationInput {
	return harness.SkillValidationInput{Proposal: harness.SkillProposal{Action: "edit", SkillName: "research", Reason: "Clarify reusable steps", Base: harness.SkillHead{Version: "base-digest", Exists: true}, CandidateVersion: "candidate-digest"}, BaseFiles: map[string][]byte{"SKILL.md": []byte("Original instructions")}, CandidateFiles: map[string][]byte{"SKILL.md": []byte("Reviewed instructions")}}
}

func TestSkillModelValidatorIsolatedProviderCallsAndEvidence(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		index := calls.Add(1)
		var request struct {
			Model               string                           `json:"model"`
			Tools               []any                            `json:"tools"`
			Messages            []struct{ Role, Content string } `json:"messages"`
			MaxCompletionTokens int                              `json:"max_completion_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		expected := "moderator"
		reason := "MODERATION_ONLY_SENTINEL"
		if index == 2 {
			expected = "evaluator"
			reason = "Clear and scoped instructions"
		}
		if request.Model != expected || len(request.Tools) != 0 || len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.MaxCompletionTokens != 512 {
			t.Errorf("unexpected provider request: %+v", request)
		}
		if strings.Contains(request.Messages[1].Content, "MODERATION_ONLY_SENTINEL") {
			t.Error("quality review inherited moderation context")
		}
		var input map[string]any
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
			t.Error(err)
		}
		decision, _ := json.Marshal(map[string]any{"base_version": input["base_version"], "candidate_version": input["candidate_version"], "passed": true, "reason": reason})
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "review", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": string(decision)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}})
	}))
	defer server.Close()
	validator, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: harness.ModelRoute{Model: "moderator", BaseURL: server.URL, APIKey: "fixture-secret"}, Evaluation: harness.ModelRoute{Model: "evaluator", BaseURL: server.URL, APIKey: "fixture-secret"}, MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	report, err := validator.Validate(context.Background(), reviewInput())
	if err != nil || calls.Load() != 2 || report.Security != "passed" || report.Behavior != "not_run" || report.IsolatedScripts || len(report.ModelChecks) != 2 {
		t.Fatalf("report: %+v %v", report, err)
	}
	for _, check := range report.ModelChecks {
		if check.InputTokens != 100 || check.OutputTokens != 20 || check.UsageEstimated || len(check.InputHash) != 64 {
			t.Fatalf("usage/digest: %+v", check)
		}
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "fixture-secret") || strings.Contains(string(data), server.URL) {
		t.Fatal("report exposed provider credentials or endpoint")
	}
}

func TestSkillModelValidatorFailureAndBounds(t *testing.T) {
	for _, mode := range []string{"rejected", "wrong-digest", "missing-passed", "duplicate", "truncated", "provider-error"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider-error" {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"message":"private-provider-secret"}}`)
					return
				}
				content := `{"base_version":"base-digest","candidate_version":"candidate-digest","passed":true,"reason":"Fixture review"}`
				finish := "stop"
				switch mode {
				case "rejected":
					content = strings.Replace(content, `"passed":true`, `"passed":false`, 1)
				case "wrong-digest":
					content = strings.Replace(content, "candidate-digest", "different-digest", 1)
				case "missing-passed":
					content = strings.Replace(content, `"passed":true,`, "", 1)
				case "duplicate":
					content = strings.Replace(content, `"passed":true,`, `"passed":false,"passed":true,`, 1)
				case "truncated":
					finish = "length"
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish}}})
			}))
			defer server.Close()
			route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "fixture-secret"}
			validator, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
			if err != nil {
				t.Fatal(err)
			}
			report, err := validator.Validate(context.Background(), reviewInput())
			if mode == "rejected" {
				if err != nil || report.Security != "failed" || len(report.ModelChecks) != 1 {
					t.Fatalf("lost rejection evidence: %+v %v", report, err)
				}
			} else if !errors.Is(err, harness.ErrSkillValidation) || strings.Contains(err.Error(), "private-provider-secret") {
				t.Fatalf("failure not closed/sanitized: %+v %v", report, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("failure caused extra model call: %d", calls.Load())
			}
			oversize := reviewInput()
			oversize.CandidateFiles["SKILL.md"] = []byte(strings.Repeat("x", 129<<10))
			if _, err = validator.Validate(context.Background(), oversize); !errors.Is(err, harness.ErrSkillValidation) || calls.Load() != 1 {
				t.Fatalf("oversize made model call: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err = validator.Validate(ctx, reviewInput()); !errors.Is(err, harness.ErrSkillValidation) || calls.Load() != 1 {
				t.Fatalf("cancelled check made request: %v", err)
			}
		})
	}
}

func TestSkillModelValidatorPreservesAdmissionStorageFailure(t *testing.T) {
	for _, phase := range []string{"reserve", "dispatch"} {
		t.Run(phase, func(t *testing.T) {
			limits := harness.BudgetLimits{MaxModelCalls: 8, MaxTokens: 100000, MaxOutputTokens: 512}
			store, ledger, _, scope := durableFixture(t, "skill-review", limits)
			trigger := `CREATE TRIGGER fail_review BEFORE INSERT ON budget_reservations BEGIN SELECT RAISE(ABORT,'review-reservation-storage-failure'); END`
			if phase == "dispatch" {
				trigger = `CREATE TRIGGER fail_review BEFORE UPDATE ON budget_reservations WHEN NEW.state='dispatched' BEGIN SELECT RAISE(ABORT,'review-dispatch-storage-failure'); END`
			}
			if _, err := store.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "unexpected provider call", 400)
			}))
			defer server.Close()
			route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "fixture"}
			validator, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
			if err != nil {
				t.Fatal(err)
			}
			work := newRunIO(context.Background())
			defer work.closeAndWait()
			runtime := &skillReviewRuntime{budget: &runBudget{limits: limits, ledger: ledger, scope: scope}, io: work, sink: &eventSink{}}
			ctx := context.WithValue(context.Background(), skillReviewRuntimeKey{}, runtime)
			_, err = validator.Validate(ctx, reviewInput())
			var persisted *durablebudget.PersistenceError
			if !errors.Is(err, harness.ErrSkillValidation) || !errors.As(work.failure(), &persisted) || calls.Load() != 0 {
				t.Fatalf("storage failure became an ordinary review failure: validation=%v run=%v calls=%d", err, work.failure(), calls.Load())
			}
		})
	}
}
