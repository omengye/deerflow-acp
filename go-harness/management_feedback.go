package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// Feedback preconditions and explicit withdrawal must survive transport
// decoding. In particular, missing/null feedback versions cannot become zero,
// and a null read list cannot become the explicit [] used by a withdrawal.
func managementFeedbackField(fields map[string]json.RawMessage, key string, out any) error {
	raw, exists := fields[key]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, out) != nil {
		return managementInvalid("invalid or missing " + key)
	}
	return nil
}

func (c *Client) managementFeedback(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	switch request.Operation {
	case "skill.feedback.get":
		if err := proposalFields(request, "signal_id"); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "signal_id")
		if err != nil {
			return nil, err
		}
		return c.evolution.Feedback(ctx, id)
	case "skill.feedback.list":
		if err := proposalFields(request, "signal_id", "cursor", "limit"); err != nil {
			return nil, err
		}
		id, err := managementID(request.Fields, "signal_id")
		if err != nil {
			return nil, err
		}
		var cursor string
		if _, exists := request.Fields["cursor"]; exists {
			if err := managementFeedbackField(request.Fields, "cursor", &cursor); err != nil {
				return nil, err
			}
			if len(cursor) > 2048 || !utf8.ValidString(cursor) || strings.ContainsRune(cursor, 0) {
				return nil, managementInvalid("invalid cursor")
			}
		}
		limit, err := optionalProposalInteger(request.Fields, "limit", 50)
		if err != nil {
			return nil, err
		}
		if limit > 100 {
			return nil, managementInvalid("limit must be 1..100")
		}
		return c.evolution.QueryFeedback(ctx, id, cursor, int(limit))
	case "skill.feedback.submit":
		if err := proposalFields(request, "signal_id", "expected_signal_state_version", "expected_feedback_version", "request_key", "kind", "summary", "read_ids"); err != nil {
			return nil, err
		}
		var feedback harness.SkillFeedbackRequest
		for _, field := range []struct {
			key string
			out any
		}{
			{"signal_id", &feedback.SignalID},
			{"expected_signal_state_version", &feedback.ExpectedSignalStateVersion},
			{"expected_feedback_version", &feedback.ExpectedFeedbackVersion},
			{"request_key", &feedback.RequestKey},
			{"kind", &feedback.Kind},
			{"summary", &feedback.Summary},
			{"read_ids", &feedback.ReadIDs},
		} {
			if err := managementFeedbackField(request.Fields, field.key, field.out); err != nil {
				return nil, err
			}
		}
		if feedback.ExpectedSignalStateVersion < 1 || feedback.ExpectedFeedbackVersion < 0 {
			return nil, managementInvalid("invalid feedback precondition")
		}
		if len(feedback.Summary) > 2000 || !utf8.ValidString(feedback.Summary) || strings.ContainsRune(feedback.Summary, 0) {
			return nil, managementInvalid("invalid summary")
		}
		if len(feedback.ReadIDs) > 16 {
			return nil, managementInvalid("read_ids must contain at most 16 IDs")
		}
		for _, id := range feedback.ReadIDs {
			if id < 1 {
				return nil, managementInvalid("invalid read_ids")
			}
		}
		return c.evolution.SubmitFeedback(ctx, feedback)
	}
	return nil, managementInvalid("unsupported skill feedback operation")
}
