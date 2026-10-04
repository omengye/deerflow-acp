package harness

import "errors"

var ErrSkillFeedbackLimit = errors.New("skill feedback revision limit reached")

// SkillFeedbackRequest records an operator's judgment about observed skill use.
// It does not establish causality or itself authorize generation/publication. Revisions
// replace the current judgment for one root/source/name/content version; they
// must never be counted as independent votes.
type SkillFeedbackRequest struct {
	SignalID                   string  `json:"signal_id"`
	ExpectedSignalStateVersion int64   `json:"expected_signal_state_version"`
	ExpectedFeedbackVersion    int64   `json:"expected_feedback_version"`
	RequestKey                 string  `json:"request_key"`
	Kind                       string  `json:"kind"`
	Summary                    string  `json:"summary"`
	ReadIDs                    []int64 `json:"read_ids"`
}

type SkillFeedback struct {
	SignalID           string      `json:"signal_id"`
	Revision           int64       `json:"revision"`
	Target             SkillTarget `json:"target"`
	SkillVersion       string      `json:"skill_version"`
	RootID             string      `json:"root_id"`
	RootSessionID      string      `json:"root_session_id"`
	SignalStateVersion int64       `json:"signal_state_version"`
	Origin             string      `json:"origin"`
	Kind               string      `json:"kind"`
	Summary            string      `json:"summary"`
	SummaryRedacted    bool        `json:"summary_redacted"`
	ReadIDs            []int64     `json:"read_ids"`
	CreatedAt          string      `json:"created_at"`
}

type SkillFeedbackSnapshot struct {
	Signal          SkillSignal    `json:"signal"`
	Feedback        *SkillFeedback `json:"feedback,omitempty"`
	EvidenceCurrent bool           `json:"evidence_current"`
}

type SkillFeedbackResult struct {
	Feedback        SkillFeedback `json:"feedback"`
	CurrentRevision int64         `json:"current_revision"`
	Replayed        bool          `json:"replayed"`
}

type SkillFeedbackPage struct {
	Revisions       []SkillFeedback `json:"revisions"`
	CurrentRevision int64           `json:"current_revision"`
	Next            string          `json:"next_cursor,omitempty"`
}
