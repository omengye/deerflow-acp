package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSkillReviewFailureDistinguishesQualityRejectionAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		retryable  bool
	}{
		{"quality-rejected", "", 0, false},
		{"quality-invalid", "review_response_invalid", 0, true},
		{"quality-truncated", "review_response_incomplete", 0, false},
		{"quality-unauthorized", "review_request_failed", 401, false},
		{"quality-rate-limited", "review_temporarily_unavailable", 429, true},
		{"quality-service-error", "review_temporarily_unavailable", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n == 2 && tc.status != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":{"message":"private-provider-secret","type":"fixture"}}`))
					return
				}
				content := `{"base_version":"base-digest","candidate_version":"candidate-digest","passed":true,"reason":"Scoped review"}`
				finish := "stop"
				if n == 2 {
					switch tc.name {
					case "quality-rejected":
						content = strings.Replace(content, `"passed":true`, `"passed":false`, 1)
					case "quality-invalid":
						content = `invalid private-provider-secret`
					case "quality-truncated":
						finish = "length"
					}
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish}}})
			}))
			defer server.Close()
			route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "fixture"}
			v, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
			if err != nil {
				t.Fatal(err)
			}
			report, err := v.Validate(context.Background(), reviewInput())
			if calls.Load() != 2 || report.Security != "passed" {
				t.Fatalf("lost security evidence or retried implicitly: %+v calls=%d", report, calls.Load())
			}
			if tc.code == "" {
				if err != nil || len(report.ModelChecks) != 2 || report.ModelChecks[1].Passed {
					t.Fatalf("quality rejection became transport failure: %+v %v", report, err)
				}
				return
			}
			var failure *harness.SkillValidationFailure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Stage != "quality" || failure.Retryable != tc.retryable || !errors.Is(err, harness.ErrSkillValidation) {
				t.Fatalf("wrong failure classification: %+v %v", failure, err)
			}
			if strings.Contains(err.Error(), "private-provider-secret") || strings.Contains(err.Error(), server.URL) || len(report.ModelChecks) != 1 {
				t.Fatalf("failed request leaked diagnostics or invented a verdict: %+v %v", report, err)
			}
		})
	}
}

func TestSkillReviewFailureNeverRetriesCancellationOrBudget(t *testing.T) {
	for _, tc := range []struct {
		cause     error
		code      string
		retryable bool
	}{
		{context.Canceled, "review_cancelled", false},
		{context.DeadlineExceeded, "review_timeout", true},
		{&budgetError{resource: "tokens"}, "review_budget_exhausted", false},
		{&budget.LimitError{Resource: "model_calls"}, "review_budget_exhausted", false},
		{skillmaintenance.ErrBudget, "review_budget_exhausted", false},
		{&net.DNSError{Err: "private-endpoint", Name: "private.example", IsTemporary: true}, "review_temporarily_unavailable", true},
		{&net.DNSError{Err: "private-endpoint", Name: "private.example", IsNotFound: true}, "review_request_failed", false},
		{errors.New("private-provider-error"), "review_request_failed", false},
	} {
		failure := skillReviewRequestFailure("quality", fmt.Errorf("private-wrapper: %w", tc.cause))
		if failure.Code != tc.code || failure.Retryable != tc.retryable || failure.Stage != "quality" || strings.Contains(failure.Error(), "private") {
			t.Fatalf("unsafe retry classification: %+v", failure)
		}
	}
}

func TestSkillReviewInputDependsOnReasonNotSource(t *testing.T) {
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			t.Errorf("unexpected input: %+v %v", req, err)
			http.Error(w, "invalid", 400)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &payload); err != nil {
			t.Error(err)
		}
		if _, found := payload["source"]; found {
			t.Error("review input contains source")
		}
		quality := strings.Contains(req.Messages[0].Content, "content quality assessment")
		passed := !quality || payload["reason"] != "Must also check output consistency"
		decision, _ := json.Marshal(map[string]any{"base_version": payload["base_version"], "candidate_version": payload["candidate_version"], "passed": passed, "reason": "Missing output consistency check."})
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": string(decision)}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "fixture"}
	v, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
	if err != nil {
		t.Fatal(err)
	}
	in := reviewInput()
	in.Proposal.Reason = "Must also check output consistency"
	var original harness.SkillValidationReport
	for i, source := range []string{"managed-evolution", "python-custom", "python-custom"} {
		in.Proposal.Target.SourceID = source
		if i == 2 {
			in.Proposal.Reason = "Diagnostic: same content, different source"
		}
		r, err := v.Validate(context.Background(), in)
		if err != nil || len(r.ModelChecks) != 2 || r.ModelChecks[1].Passed != (i == 2) {
			t.Fatalf("unexpected result: %+v %v", r, err)
		}
		if i == 0 {
			original = r
			continue
		}
		for j, c := range r.ModelChecks {
			if (c.InputHash == original.ModelChecks[j].InputHash) != (i == 1) {
				t.Fatal("review identity does not distinguish changed reason from changed source")
			}
		}
	}
}
