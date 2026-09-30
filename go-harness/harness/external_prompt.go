package harness

// ExternalPromptState identifies an external ACP prompt whose remote result
// has not yet been matched to a committed local tool receipt.
type ExternalPromptState struct {
	Agent        string `json:"agent"`
	SessionID    string `json:"sessionId"`
	PromptID     string `json:"promptId"`
	PromptSHA    string `json:"promptSha"`
	ArgumentsSHA string `json:"argumentsSha,omitempty"`
	RunID        string `json:"runId,omitempty"`
	ToolCallID   string `json:"toolCallId,omitempty"`
}
