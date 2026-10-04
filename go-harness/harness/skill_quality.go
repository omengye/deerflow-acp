package harness

// Quality assessment separates host-observed causes from a human's explicit
// quality judgment. Ordinary failures and model text never grant high confidence.
type SkillQualityAssessment struct {
	Kind             string  `json:"kind"`
	Confidence       string  `json:"confidence"`
	Source           string  `json:"source"`
	Eligible         bool    `json:"eligible"`
	FeedbackRevision int64   `json:"feedback_revision,omitempty"`
	EvidenceReadIDs  []int64 `json:"evidence_read_ids,omitempty"`
}
