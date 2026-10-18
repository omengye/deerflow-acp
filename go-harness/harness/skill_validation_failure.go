package harness

// SkillValidationFailure is host-selected diagnostic metadata, not approval
// evidence. Retryable permits consideration of a bounded retry under the same
// authorization and budget; it neither schedules one nor resets a failed job.
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
	case "review_budget_exhausted":
		return "The review budget is exhausted."
	case "review_storage_error":
		return "Review accounting or storage failed."
	case "review_input_too_large":
		return "The candidate review input exceeds the configured bounds."
	case "review_configuration_error":
		return "The review configuration or request options are invalid."
	case "review_response_invalid":
		return "The review response is invalid or refers to different content; no decision was accepted."
	case "review_response_incomplete":
		return "The review response was incomplete; no decision was accepted."
	case "review_refused":
		return "The review provider refused the request; no decision was accepted and review retry is not permitted."
	case "review_request_failed":
		return "The review model request failed; this does not establish a content rejection."
	case "review_temporarily_unavailable":
		return "The review model request encountered a transient network, rate-limit or service failure."
	default:
		return "Candidate validation is incomplete. Inspect the report; do not infer a source outage."
	}
}
