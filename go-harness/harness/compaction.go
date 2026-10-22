package harness

// CompactionConfig controls Eino's native conversation summarization. It only
// changes the model-facing session view; business events and receipts remain
// append-only. A zero token threshold follows the selected model's context
// window (32,000-token fallback); zero messages disables the count trigger.
type CompactionConfig struct {
	Enabled            bool
	ContextMessages    int
	ContextTokens      int
	KeepRecentMessages int
}

const (
	CompactionTruncated = "truncated"
	CompactionEmpty     = "empty"
	CompactionOversized = "oversized"
	CompactionInvalid   = "invalid"
)

// CompactionError describes a rejected summary without retaining its content
// or reasoning. The original conversation must remain intact on this failure.
type CompactionError struct {
	Reason       string
	FinishReason string
	SummaryBytes int
	Attempts     int
}

func (e *CompactionError) Error() string {
	reason := "the summary was invalid"
	if e != nil {
		switch e.Reason {
		case CompactionTruncated:
			reason = "the summary reached its output limit"
		case CompactionEmpty:
			reason = "the model returned an empty summary"
		case CompactionOversized:
			reason = "the summary exceeded the safe size limit"
		}
	}
	return "Context compaction failed because " + reason + ". The original conversation was preserved. Review completed tool results before continuing, or start a new conversation with a short handoff."
}
