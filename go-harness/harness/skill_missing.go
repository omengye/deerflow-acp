package harness

import "errors"

var ErrSkillMissingReportLimit = errors.New("missing skill report revision limit reached")

// SkillMissingReportRequest is an operator's missing-capability judgment for a
// real task. SessionID asserts the foreground root session; RunID may identify
// its root or a saved descendant. Neither field grants a connection ownership.
type SkillMissingReportRequest struct {
	SessionID             string `json:"session_id"`
	RunID                 string `json:"run_id"`
	ExpectedReportVersion int64  `json:"expected_report_version"`
	RequestKey            string `json:"request_key"`
	Kind                  string `json:"kind"`
	Summary               string `json:"summary"`
}

// SkillMissingReport does not invent a skill target, version or access event.
// All revisions for one root replace one judgment, never independent votes.
type SkillMissingReport struct {
	ID              string `json:"id"`
	RootID          string `json:"root_id"`
	RootSessionID   string `json:"root_session_id"`
	RunID           string `json:"run_id"`
	Revision        int64  `json:"revision"`
	Origin          string `json:"origin"`
	Kind            string `json:"kind"`
	Summary         string `json:"summary"`
	SummaryRedacted bool   `json:"summary_redacted"`
	Attribution     string `json:"attribution"`
	CreatedAt       string `json:"created_at"`
}

type SkillMissingReportSnapshot struct {
	RootID        string              `json:"root_id"`
	RootSessionID string              `json:"root_session_id"`
	Report        *SkillMissingReport `json:"report,omitempty"`
}

type SkillMissingReportResult struct {
	Report          SkillMissingReport `json:"report"`
	CurrentRevision int64              `json:"current_revision"`
	Replayed        bool               `json:"replayed"`
}

type SkillMissingReportPage struct {
	Reports        []SkillMissingReport `json:"reports"`
	CatalogVersion int64                `json:"catalog_version"`
	Next           string               `json:"next_cursor,omitempty"`
}

type SkillMissingReportHistoryPage struct {
	Revisions       []SkillMissingReport `json:"revisions"`
	CurrentRevision int64                `json:"current_revision"`
	Next            string               `json:"next_cursor,omitempty"`
}
