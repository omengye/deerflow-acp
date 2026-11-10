package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/openai"
	openaisdk "github.com/meguminnnnnnnnn/go-openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Only recognized transient causes permit a retry. Raw provider diagnostics
// never enter the proposal, tool result or public error chain.
func skillReviewRequestFailure(stage string, err error) *harness.SkillValidationFailure {
	f := &harness.SkillValidationFailure{Code: "review_request_failed", Stage: stage, DetailCode: "cause_unavailable"}
	var known *harness.SkillValidationFailure
	var quota *budget.LimitError
	var localQuota *budgetError
	var persistence interface{ PersistenceFailure() bool }
	var network net.Error
	switch {
	case errors.As(err, &known):
		*f = *known
		f.Stage = stage
	case errors.As(err, &persistence) && persistence.PersistenceFailure():
		f.Code = "review_storage_error"
	case errors.As(err, &quota), errors.As(err, &localQuota), errors.Is(err, skillmaintenance.ErrCapacity):
		f.Code = "review_runtime_unavailable"
	case errors.Is(err, context.Canceled):
		f.Code = "review_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		f.Code, f.Retryable = "review_timeout", true
	case errors.Is(err, harness.ErrContextLimit):
		f.Code, f.DetailCode = "review_input_too_large", "model_context_limit"
	default:
		// A recognized HTTP response takes precedence over its body decoder's
		// error. Compatible endpoints often return HTML/plain text on failure;
		// the SDK retains that status in RequestError instead of APIError.
		if status := skillReviewHTTPStatus(err); status >= 400 && status <= 599 {
			f.DetailCode = fmt.Sprintf("http_%d", status)
			if status == 408 || status == 429 || status >= 500 {
				f.Code, f.Retryable = "review_temporarily_unavailable", true
			}
		} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
			f.Code, f.Retryable, f.DetailCode = "review_temporarily_unavailable", true, "connection_interrupted"
		} else if errors.As(err, &network) {
			f.DetailCode = "network_error"
			if network.Timeout() {
				f.Code, f.Retryable, f.DetailCode = "review_temporarily_unavailable", true, "network_timeout"
			} else if network.Temporary() {
				f.Code, f.Retryable, f.DetailCode = "review_temporarily_unavailable", true, "network_temporarily_unavailable"
			}
			var dns *net.DNSError
			if errors.As(err, &dns) && dns.IsNotFound {
				f.DetailCode = "hostname_not_found"
			}
		}
	}
	if f.Code != "review_request_failed" && f.Code != "review_temporarily_unavailable" && f.DetailCode == "cause_unavailable" {
		f.DetailCode = ""
	}
	if f.Code == "review_request_failed" && f.DetailCode == "" {
		f.DetailCode = "cause_unavailable"
	}
	return f
}

func skillReviewHTTPStatus(err error) int {
	var openAI *openai.APIError
	var request *openaisdk.RequestError
	var streamAPI *openaisdk.APIError
	var claude *anthropic.Error
	switch {
	case errors.As(err, &openAI):
		return openAI.HTTPStatusCode
	case errors.As(err, &request):
		return request.HTTPStatusCode
	case errors.As(err, &streamAPI):
		return streamAPI.HTTPStatusCode
	case errors.As(err, &claude):
		return claude.StatusCode
	default:
		return 0
	}
}
