package agent

import (
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const compactionErrorCode = -32025

func compactionError(err *harness.CompactionError) error {
	reason, finish, size, attempts := harness.CompactionInvalid, "unknown", 0, 1
	if err != nil {
		switch err.Reason {
		case harness.CompactionTruncated, harness.CompactionEmpty, harness.CompactionOversized:
			reason = err.Reason
		}
		switch err.FinishReason {
		case "length", "max_tokens", "stop", "end_turn", "stop_sequence", "tool_calls", "tool_use", "content_filter", "refusal":
			finish = err.FinishReason
		}
		size, attempts = max(0, err.SummaryBytes), max(1, err.Attempts)
	}
	return &protocol.Error{Code: compactionErrorCode, Message: err.Error(), Data: map[string]any{
		"kind": "context_compaction", "reason": reason, "finishReason": finish,
		"summaryBytes": size, "attempts": attempts, "automaticReplay": false,
	}}
}
