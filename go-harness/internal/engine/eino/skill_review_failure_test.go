package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	openaisdk "github.com/meguminnnnnnnnn/go-openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestSkillReviewHTTPFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		retryable   bool
		maintenance bool
	}{
		{"invalid-request", 400, `{"error":{"message":"private-provider-secret","param":"private-param"}}`, false, false},
		{"unauthorized", 401, `{"error":{"message":"private-provider-secret"}}`, false, false},
		{"forbidden-plain-text", 403, "private-provider-secret", false, false},
		{"missing-model", 404, `{"message":"private-provider-secret"}`, false, false},
		{"request-timeout", 408, "private-provider-secret", true, false},
		{"unprocessable-request", 422, "private-provider-secret", false, false},
		{"rate-limit-plain-text", 429, "private-provider-secret", true, false},
		{"gateway-html", 502, "<html>private-provider-secret</html>", true, false},
		{"service-malformed-json", 503, `{"error":private-provider-secret}`, true, false},
		{"maintenance-gateway-html", 502, "<html>private-provider-secret</html>", true, true},
		{"maintenance-unauthorized", 401, "private-provider-secret", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			ctx := context.Background()
			if tc.maintenance {
				_, _, _, ctx = maintenanceReviewFixture(t, 3)
			}
			route := harness.ModelRoute{Model: "reviewer", BaseURL: server.URL, APIKey: "private-api-key"}
			validator, err := NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
			if err != nil {
				t.Fatal(err)
			}
			report, err := validator.Validate(ctx, reviewInput())
			var failure *harness.SkillValidationFailure
			if !errors.As(err, &failure) {
				t.Fatalf("missing structured failure: %v", err)
			}
			wantCode := "review_request_failed"
			if tc.retryable {
				wantCode = "review_temporarily_unavailable"
			}
			if failure.Code != wantCode || failure.Stage != "security" || failure.Retryable != tc.retryable || failure.DetailCode != fmt.Sprintf("http_%d", tc.status) {
				t.Fatalf("incorrect failure: %+v", failure)
			}
			if calls.Load() != 1 || report.Security != "unavailable" || report.Behavior != "not_run" || len(report.ModelChecks) != 0 {
				t.Fatalf("failed request retried or invented a review: calls=%d report=%+v", calls.Load(), report)
			}
			encoded, marshalErr := json.Marshal(failure)
			if marshalErr != nil || strings.Contains(string(encoded)+err.Error(), "private-") || strings.Contains(string(encoded)+err.Error(), server.URL) || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
				t.Fatalf("unsafe or opaque failure: %s %v", encoded, err)
			}
			var raw *openaisdk.RequestError
			if errors.As(err, &raw) {
				t.Fatal("raw provider diagnostics escaped in error chain")
			}
		})
	}
}

func TestSkillReviewRequestFailureRecognizesTypedCauses(t *testing.T) {
	for _, tc := range []struct {
		name, code, detail string
		cause              error
		retryable          bool
	}{
		{"api-rate-limit", "review_temporarily_unavailable", "http_429", &openaiapi.APIError{HTTPStatusCode: 429, Message: "private-secret"}, true},
		{"sdk-stream-service", "review_temporarily_unavailable", "http_503", &openaisdk.APIError{HTTPStatusCode: 503, Message: "private-secret"}, true},
		{"request-status-before-nested-error", "review_temporarily_unavailable", "http_503", &openaisdk.RequestError{HTTPStatusCode: 503, Err: &net.DNSError{IsNotFound: true, Name: "private-secret"}}, true},
		{"request-status-before-eof", "review_request_failed", "http_400", &openaisdk.RequestError{HTTPStatusCode: 400, Err: io.EOF}, false},
		{"anthropic-service", "review_temporarily_unavailable", "http_529", &anthropic.Error{StatusCode: 529}, true},
		{"interrupted-stream", "review_temporarily_unavailable", "connection_interrupted", io.ErrUnexpectedEOF, true},
		{"connection-closed", "review_temporarily_unavailable", "connection_interrupted", io.EOF, true},
		{"connection-reset", "review_temporarily_unavailable", "connection_interrupted", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"broken-pipe", "review_temporarily_unavailable", "connection_interrupted", syscall.EPIPE, true},
		{"dns-not-found", "review_request_failed", "hostname_not_found", &net.DNSError{IsNotFound: true, Name: "private-secret"}, false},
		{"unknown-is-not-a-status", "review_request_failed", "cause_unavailable", errors.New("private-secret: status code 503"), false},
		{"unclassified-wrapper", "review_request_failed", "cause_unavailable", &harness.SkillValidationFailure{Code: "review_request_failed"}, false},
		{"context-limit", "review_input_too_large", "model_context_limit", harness.ErrContextLimit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := skillReviewRequestFailure("security", fmt.Errorf("private-wrapper: %w", tc.cause))
			if failure.Code != tc.code || failure.DetailCode != tc.detail || failure.Retryable != tc.retryable || strings.Contains(failure.Error(), "private-") {
				t.Fatalf("incorrect classification: %+v", failure)
			}
		})
	}
}

func TestSkillReviewModelInitializationFailureIsActionable(t *testing.T) {
	route := harness.ModelRoute{Provider: "private-unsupported-provider", Model: "private-model"}
	_, err := NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
	var failure *harness.SkillValidationFailure
	if !errors.As(err, &failure) || failure.Code != "review_configuration_error" || failure.DetailCode != "model_initialization_failed" || failure.Retryable || strings.Contains(err.Error(), "private-") {
		t.Fatalf("incorrect initialization diagnostic: %+v %v", failure, err)
	}
}
