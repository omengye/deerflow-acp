package eino

import (
	"context"
	"errors"
	"net"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Only recognized transient causes permit a retry. Raw provider diagnostics
// never enter the proposal, tool result or public error chain.
func skillReviewRequestFailure(stage string, err error) *harness.SkillValidationFailure {
	f := &harness.SkillValidationFailure{Code: "review_request_failed", Stage: stage}
	var known *harness.SkillValidationFailure
	var quota *budget.LimitError
	var localQuota *budgetError
	var persistence interface{ PersistenceFailure() bool }
	var network net.Error
	switch {
	case errors.As(err, &known):
		*f = *known
		f.Stage = stage
	case errors.As(err, &persistence):
		f.Code = "review_storage_error"
	case errors.As(err, &quota), errors.As(err, &localQuota), errors.Is(err, skillmaintenance.ErrBudget):
		f.Code = "review_budget_exhausted"
	case errors.Is(err, context.Canceled):
		f.Code = "review_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		f.Code, f.Retryable = "review_timeout", true
	case errors.As(err, &network):
		if network.Timeout() || network.Temporary() {
			f.Code, f.Retryable = "review_temporarily_unavailable", true
		}
	default:
		status := 0
		var openAI *openai.APIError
		var claude *anthropic.Error
		if errors.As(err, &openAI) {
			status = openAI.HTTPStatusCode
		} else if errors.As(err, &claude) {
			status = claude.StatusCode
		}
		if status == 408 || status == 429 || status >= 500 && status <= 599 {
			f.Code, f.Retryable = "review_temporarily_unavailable", true
		}
	}
	return f
}
