package eino

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type reviewParseFailure string

func (e reviewParseFailure) Error() string { return string(e) }

func invalidReviewResponse(stage, detail, content string, retry bool) *harness.SkillValidationFailure {
	return &harness.SkillValidationFailure{Code: "review_response_invalid", Stage: stage, Retryable: retry, DetailCode: detail, ResponseDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), ResponseBytes: len(content)}
}

func validateReviewResponse(answer *schema.Message, report harness.SkillValidationReport, stage string) (skillDecision, error) {
	if answer == nil {
		return skillDecision{}, invalidReviewResponse(stage, "empty_response", "", true)
	}
	if len(answer.ToolCalls) > 0 {
		return skillDecision{}, invalidReviewResponse(stage, "unexpected_tool_calls", answer.Content, false)
	}
	if len(answer.Content) > 32<<10 {
		return skillDecision{}, invalidReviewResponse(stage, "response_too_large", answer.Content, false)
	}
	if answer.ResponseMeta != nil && answer.ResponseMeta.FinishReason != "" && answer.ResponseMeta.FinishReason != "stop" && answer.ResponseMeta.FinishReason != "end_turn" {
		failure := invalidReviewResponse(stage, "unexpected_finish_reason", answer.Content, false)
		failure.Code = "review_response_incomplete"
		switch answer.ResponseMeta.FinishReason {
		case "content_filter", "refusal":
			failure.Code, failure.DetailCode = "review_refused", "provider_refusal"
		case "length", "max_tokens":
			failure.DetailCode = "output_truncated"
		}
		return skillDecision{}, failure
	}
	content := strings.TrimSpace(answer.Content)
	// Accept exactly one enclosing fence, not prose plus an extracted fragment.
	if strings.HasPrefix(content, "```json\n") || strings.HasPrefix(content, "```\n") || strings.HasPrefix(content, "```json\r\n") {
		_, tail, _ := strings.Cut(content, "\n")
		if strings.HasSuffix(tail, "\n```") {
			content = strings.TrimSpace(strings.TrimSuffix(tail, "\n```"))
		}
	}
	decision, err := parseSkillDecision(content)
	if err != nil {
		detail := "invalid_json"
		var classified reviewParseFailure
		if errors.As(err, &classified) {
			detail = string(classified)
		}
		return decision, invalidReviewResponse(stage, detail, answer.Content, detail != "duplicate_field")
	}
	if decision.BaseVersion != report.BaseVersion || decision.CandidateVersion != report.CandidateVersion {
		return decision, invalidReviewResponse(stage, "version_mismatch", answer.Content, false)
	}
	return decision, nil
}
