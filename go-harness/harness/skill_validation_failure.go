package harness

import (
	"fmt"
	"strconv"
	"strings"
)

// SkillValidationFailure is host-selected diagnostic metadata, not approval
// evidence. Retryable permits consideration of a bounded retry under the same
// authorization and execution limits; it neither schedules one nor resets a failed job.
type SkillValidationFailure struct {
	Code           string `json:"code"`
	Stage          string `json:"stage"`
	Retryable      bool   `json:"retryable"`
	DetailCode     string `json:"detail_code,omitempty"`
	ResponseDigest string `json:"response_digest,omitempty"`
	ResponseBytes  int    `json:"response_bytes,omitempty"`
}

func (f *SkillValidationFailure) Unwrap() error { return ErrSkillValidation }

func (f *SkillValidationFailure) Error() string {
	switch f.Code {
	case "security_rejected":
		return "Candidate security review rejected the content. Inspect model_checks for the reason."
	case "quality_rejected":
		return "Candidate quality review rejected the content. Inspect model_checks for the reason."
	case "content_rejected":
		return "A candidate content check did not pass. Inspect model_checks for the reason."
	case "behavior_failed":
		return "Candidate behavior evaluation did not pass. Inspect the behavior report."
	case "behavior_unavailable":
		return "Candidate behavior evaluation could not complete."
	case "script_validation_required":
		return "Executable support files require successful isolated behavior validation."
	case "validator_unavailable":
		return "No candidate validator is configured."
	case "invalid_validation_report":
		return "The validation report is invalid or not bound to this candidate."
	case "validation_unavailable":
		return "Candidate validation could not complete; no content rejection was established."
	case "review_timeout":
		return "The review request timed out before a decision was available."
	case "review_cancelled":
		return "The review request was cancelled before a decision was available."
	case "review_runtime_unavailable":
		return "The review runtime is unavailable."
	case "review_storage_error":
		return "Review accounting or storage failed."
	case "review_input_too_large":
		return "The candidate review input exceeds the configured bounds."
	case "review_configuration_error":
		if f.DetailCode == "model_initialization_failed" {
			return "The configured review model could not be initialized. Check its provider and request options."
		}
		return "The review configuration or request options are invalid."
	case "review_response_invalid":
		return "The review response is invalid or refers to different content; no decision was accepted."
	case "review_response_incomplete":
		return "The review response was incomplete; no decision was accepted."
	case "review_refused":
		return "The review provider refused the request; no decision was accepted and review retry is not permitted."
	case "review_request_failed":
		if message := reviewRequestFailureMessage(f.DetailCode); message != "" {
			return message
		}
		return "The review model request failed; this does not establish a content rejection."
	case "review_temporarily_unavailable":
		if message := reviewRequestFailureMessage(f.DetailCode); message != "" {
			return message
		}
		return "The review model request encountered a transient network, rate-limit or service failure."
	default:
		return "Candidate validation is incomplete. Inspect the report; do not infer a source outage."
	}
}

// Only host-selected categories and bounded numeric HTTP statuses are rendered.
// Provider messages, bodies, URLs and arbitrary detail strings are never echoed.
func reviewRequestFailureMessage(detail string) string {
	switch detail {
	case "connection_interrupted":
		return "The review connection ended before a decision was available. A bounded review retry may be considered."
	case "network_timeout":
		return "The review network request timed out before a decision was available."
	case "network_temporarily_unavailable":
		return "The review request encountered a temporary network failure before a decision was available."
	case "hostname_not_found":
		return "The configured review endpoint hostname could not be resolved. Check the endpoint configuration."
	case "network_error":
		return "The review request could not reach its endpoint. The network failure was not classified as transient."
	}
	if !strings.HasPrefix(detail, "http_") {
		return ""
	}
	status, err := strconv.Atoi(strings.TrimPrefix(detail, "http_"))
	if err != nil || status < 400 || status > 599 {
		return ""
	}
	reason := "The provider rejected the review request."
	switch status {
	case 400, 422:
		reason = "The provider rejected the review request as invalid. Check the review model and request options."
	case 401:
		reason = "The review provider rejected authentication. Check the configured credentials."
	case 403:
		reason = "The review provider denied access. Check account and model permissions."
	case 404:
		reason = "The review endpoint or model was not found. Check the endpoint and model configuration."
	case 405:
		reason = "The review endpoint does not accept this request method. Check the endpoint configuration."
	case 408:
		reason = "The provider timed out the review request."
	case 413:
		reason = "The provider rejected the review request as too large."
	case 429:
		reason = "The provider limited the review request. Check rate limits and account capacity."
	default:
		if status >= 500 {
			reason = "The review provider returned a service error."
		}
	}
	return fmt.Sprintf("%s (HTTP %d.) No content review decision was accepted.", reason, status)
}
