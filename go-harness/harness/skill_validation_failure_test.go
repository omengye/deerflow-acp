package harness

import (
	"strings"
	"testing"
)

func TestSkillValidationRequestFailureMessagesAreHostSelected(t *testing.T) {
	for _, tc := range []struct {
		detail, want string
	}{
		{"http_400", "request as invalid"},
		{"http_401", "authentication"},
		{"http_403", "denied access"},
		{"http_404", "endpoint or model was not found"},
		{"http_429", "rate limits"},
		{"http_503", "service error"},
		{"connection_interrupted", "connection ended"},
		{"hostname_not_found", "hostname could not be resolved"},
		{"cause_unavailable", "does not establish a content rejection"},
		{"private-provider-message", "does not establish a content rejection"},
		{"http_401 private-provider-message", "does not establish a content rejection"},
		{"http_999", "does not establish a content rejection"},
	} {
		failure := &SkillValidationFailure{Code: "review_request_failed", DetailCode: tc.detail}
		if message := failure.Error(); !strings.Contains(message, tc.want) || strings.Contains(message, "private-") {
			t.Fatalf("unexpected diagnostic for %q: %q", tc.detail, message)
		}
	}
}
