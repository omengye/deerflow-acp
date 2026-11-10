package harness

// Recovery restores an already published package's installation registration.
// These types are host management inputs, never model tool arguments.
type SkillRecoveryPlan struct {
	Target        SkillTarget      `json:"target"`
	Version       string           `json:"version"`
	Enabled       bool             `json:"enabled"`
	ReceiptSHA256 string           `json:"receipt_sha256"`
	Files         []SkillFileInfo  `json:"files"`
	Publication   SkillPublication `json:"historical_publication"`
}

type SkillRecovery struct {
	Target          SkillTarget `json:"target"`
	HistoryDatabase string      `json:"history_database"`
	Version         string      `json:"expected_version"`
	ReceiptSHA256   string      `json:"expected_receipt_sha256"`
	Enabled         bool        `json:"enabled"`
	RequestKey      string      `json:"request_key"`
	Actor           string      `json:"actor"`
}
