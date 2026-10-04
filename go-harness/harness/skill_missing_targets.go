package harness

// SkillMissingReportTarget identifies an accepted root task without requiring
// a skill read or an existing missing-capability report. Status describes the
// original root run; it does not summarize the outcome of its descendants.
type SkillMissingReportTarget struct {
	RootID         string `json:"root_id"`
	RootSessionID  string `json:"root_session_id"`
	RootRunID      string `json:"root_run_id"`
	CreatedAt      string `json:"created_at"`
	Status         string `json:"status"`
	ReportRevision int64  `json:"report_revision"`
}

// SkillMissingReportTargetPage is a live, bounded view ordered by original
// root-run creation time and root identity, newest first. Refresh from the
// beginning to see newer tasks. Changes in status do not reorder the list.
type SkillMissingReportTargetPage struct {
	Targets []SkillMissingReportTarget `json:"targets"`
	Next    string                     `json:"next_cursor,omitempty"`
}
