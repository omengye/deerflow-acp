package evolution

import (
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func failedCheck(report Report) *harness.SkillValidationFailure {
	if report.Security == "failed" {
		return &harness.SkillValidationFailure{Code: "security_rejected", Stage: "security"}
	}
	for _, check := range report.ModelChecks {
		if check.Passed {
			continue
		}
		switch check.Stage {
		case "security", "quality":
			return &harness.SkillValidationFailure{Code: check.Stage + "_rejected", Stage: check.Stage}
		default:
			return &harness.SkillValidationFailure{Code: "content_rejected", Stage: "content"}
		}
	}
	if report.Behavior == "failed" {
		return &harness.SkillValidationFailure{Code: "behavior_failed", Stage: "behavior"}
	}
	return nil
}

func validationFailure(report Report, input ValidationInput, reviewErr error, configured bool) *harness.SkillValidationFailure {
	if !configured {
		return &harness.SkillValidationFailure{Code: "validator_unavailable", Stage: "validation"}
	}
	if !reportBoundTo(report, input) {
		return &harness.SkillValidationFailure{Code: "invalid_validation_report", Stage: "validation"}
	}
	if reviewErr != nil {
		var failure *harness.SkillValidationFailure
		if errors.As(reviewErr, &failure) {
			copy := *failure
			return &copy
		}
		stage, code := "validation", "validation_unavailable"
		if report.Behavior == "unavailable" {
			stage, code = "behavior", "behavior_unavailable"
		}
		return &harness.SkillValidationFailure{Code: code, Stage: stage}
	}
	if failure := failedCheck(report); failure != nil {
		return failure
	}
	if report.Security == "unavailable" || report.Behavior == "unavailable" {
		return &harness.SkillValidationFailure{Code: "validation_unavailable", Stage: "validation"}
	}
	if containsScripts(input.CandidateFiles) && (!report.IsolatedScripts || report.Behavior != "passed") {
		return &harness.SkillValidationFailure{Code: "script_validation_required", Stage: "behavior"}
	}
	return &harness.SkillValidationFailure{Code: "validation_incomplete", Stage: "validation"}
}

type ModelCheckFeedback struct {
	Stage          string `json:"stage"`
	Passed         bool   `json:"passed"`
	Reason         string `json:"reason"`
	ReasonRedacted bool   `json:"reason_redacted,omitempty"`
	InputHash      string `json:"input_hash,omitempty"`
}

type ValidationFeedback struct {
	Quality    string
	Failure    *harness.SkillValidationFailure
	Checks     []ModelCheckFeedback
	Message    string
	NextAction string
}

// ProposalFeedback projects legacy reports too, without rewriting their hashes
// or saved receipts. Model-authored explanations remain untrusted evidence.
func ProposalFeedback(p Proposal) ValidationFeedback {
	f := ValidationFeedback{Quality: "unknown", Checks: []ModelCheckFeedback{}}
	if p.Status == "validating" || p.Evaluation.Security == "failed" {
		f.Quality = "not_run"
	}
	for i, check := range p.Evaluation.ModelChecks {
		if i == 8 {
			break
		}
		if check.Stage == "quality" {
			if !check.Passed {
				f.Quality = "failed"
			} else if f.Quality != "failed" {
				f.Quality = "passed"
			}
		}
		stage := check.Stage
		if stage != "security" && stage != "quality" {
			stage = "content"
		}
		reason, redacted, err := cleanFeedbackSummary(check.Reason)
		if err != nil {
			reason, redacted = "Review explanation omitted because it exceeds safe text bounds.", true
		}
		digest := check.InputHash
		if !hexDigest(digest) {
			digest = ""
		}
		f.Checks = append(f.Checks, ModelCheckFeedback{Stage: stage, Passed: check.Passed, Reason: reason, ReasonRedacted: redacted, InputHash: digest})
	}
	if p.Status != "failed" {
		return f
	}
	if p.ValidationFailure != nil {
		copy := *p.ValidationFailure
		f.Failure = &copy
	} else {
		// Historical failures do not preserve the transport cause. Only an
		// explicit failed check proves rejection; absence cannot prove outage.
		f.Failure = failedCheck(p.Evaluation)
		if f.Failure == nil {
			f.Failure = &harness.SkillValidationFailure{Code: "validation_incomplete", Stage: "validation"}
		}
	}
	f.Message = f.Failure.Error()
	if f.Failure.Code == "validation_incomplete" && p.Error != "" && p.Error != "Candidate validation was unavailable or did not pass." {
		if message, _, err := cleanFeedbackSummary(p.Error); err == nil && message != "" {
			f.Message = message
		}
	}
	if f.Quality == "unknown" && f.Failure.Stage == "quality" {
		f.Quality = "unavailable"
	} else if f.Quality == "unknown" && p.ValidationFailure != nil && f.Failure.Stage == "security" {
		f.Quality = "not_run"
	}
	f.NextAction = "Inspect the proposal report or ask the operator to diagnose the failure. Do not infer a source outage or resubmit unchanged content to another source."
	switch f.Failure.Code {
	case "security_rejected", "quality_rejected", "content_rejected":
		f.NextAction = "Address the failed checks in the candidate, preserve the original problem and unmet requirements in reason, then submit the revision to the same authorized source. Do not change source or weaken reason to obtain a passing review."
	default:
		if CanRetryReview(p) {
			f.NextAction = "Ask the operator to use proposal.review_retry when available. It creates one budgeted successor with identical source, content, base and reason; it does not publish. Do not bypass the bound with a fresh request key or another source. Stop if the successor fails."
		}
	}
	return f
}
