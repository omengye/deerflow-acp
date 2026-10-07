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
