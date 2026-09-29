package harness

// CompactionConfig controls Eino's native conversation summarization. It only
// changes the model-facing session view; business events and receipts remain
// append-only. Zero-valued thresholds receive defaults when Enabled is true.
type CompactionConfig struct {
	Enabled            bool
	ContextMessages    int
	ContextTokens      int
	KeepRecentMessages int
}
