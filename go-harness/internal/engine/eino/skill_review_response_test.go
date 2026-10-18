package eino

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestReviewResponseStrictEnvelopeAndPrivateDiagnostics(t *testing.T) {
	content := `{"base_version":"","candidate_version":"sha","passed":true,"reason":"Scoped review"}`
	report := harness.SkillValidationReport{CandidateVersion: "sha"}
	for _, tc := range []struct {
		name, body, detail string
		retry              bool
	}{
		{"object", content, "", false}, {"fenced", "```json\n" + content + "\n```", "", false}, {"crlf", "```json\r\n" + content + "\r\n```", "", false},
		{"prose", "private prefix " + content, "invalid_json", true},
		{"duplicate", strings.Replace(content, `"passed":true`, `"passed":false,"passed":true`, 1), "duplicate_field", false},
		{"mismatch", strings.Replace(content, `"sha"`, `"different"`, 1), "version_mismatch", false},
		{"null", strings.Replace(content, `"passed":true`, `"passed":null`, 1), "missing_or_null_field", true},
		{"type", strings.Replace(content, `"passed":true`, `"passed":"true"`, 1), "field_type_invalid", true},
		{"reason", strings.Replace(content, "Scoped review", strings.Repeat("x", 2001), 1), "reason_invalid_or_too_long", true},
		{"trailing", content + " private suffix", "unexpected_fields_or_trailing_output", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateReviewResponse(&schema.Message{Content: tc.body}, report, "security")
			if tc.detail == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var f *harness.SkillValidationFailure
			if !errors.As(err, &f) || f.DetailCode != tc.detail || f.Retryable != tc.retry || len(f.ResponseDigest) != 64 || f.ResponseBytes != len(tc.body) {
				t.Fatalf("%+v", f)
			}
			b, _ := json.Marshal(f)
			if strings.Contains(string(b), "private") || strings.Contains(f.Error(), "private") {
				t.Fatal("raw response leaked")
			}
		})
	}
}

func TestReviewStructuredResponseFormatOnWire(t *testing.T) {
	calls := 0
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		format, ok := req["response_format"].(map[string]any)
		if !ok || format["type"] != "json_schema" {
			t.Error("structured response option missing", req["response_format"])
		}
		calls++
		content := `{"base_version":"base-digest","candidate_version":"candidate-digest","passed":true,"reason":"Scoped review"}`
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	route := harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "fixture"}
	v, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route, ResponseFormat: "json_schema"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Validate(context.Background(), reviewInput()); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestReviewFinishReasonsNeverRetryRefusal(t *testing.T) {
	for _, tc := range []struct{ finish, code, detail string }{
		{"content_filter", "review_refused", "provider_refusal"},
		{"refusal", "review_refused", "provider_refusal"},
		{"length", "review_response_incomplete", "output_truncated"},
		{"max_tokens", "review_response_incomplete", "output_truncated"},
		{"unknown-private-reason", "review_response_incomplete", "unexpected_finish_reason"},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			_, err := validateReviewResponse(&schema.Message{Content: "private response", ResponseMeta: &schema.ResponseMeta{FinishReason: tc.finish}}, harness.SkillValidationReport{}, "security")
			var failure *harness.SkillValidationFailure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.DetailCode != tc.detail || failure.Retryable || len(failure.ResponseDigest) != 64 {
				t.Fatalf("unexpected diagnostic: %+v", failure)
			}
			data, _ := json.Marshal(failure)
			if strings.Contains(string(data), "private") {
				t.Fatal("private response or provider finish reason leaked")
			}
		})
	}
}
