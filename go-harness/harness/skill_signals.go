package harness

// SkillSignal is one observed skill version within one root task. Counts are
// evidence inventory, not quality scores: a read or completed run alone does
// not establish that this skill caused success/failure.
type SkillSignal struct {
	ID                string                 `json:"id"`
	Target            SkillTarget            `json:"target"`
	Version           string                 `json:"version"`
	RootID            string                 `json:"root_id"`
	RootSessionID     string                 `json:"root_session_id"`
	StateVersion      int64                  `json:"state_version"`
	FeedbackRevision  int64                  `json:"feedback_revision"`
	Attribution       string                 `json:"attribution"`
	ReadCount         int64                  `json:"read_count"`
	RunCount          int64                  `json:"run_count"`
	RoleCount         int64                  `json:"role_count"`
	CompletedRuns     int64                  `json:"completed_runs"`
	FailedRuns        int64                  `json:"failed_runs"`
	CancelledRuns     int64                  `json:"cancelled_runs"`
	UncertainRuns     int64                  `json:"uncertain_runs"`
	CollectionPending bool                   `json:"collection_pending"`
	Quality           SkillQualityAssessment `json:"quality"`
}

type SkillSignalPage struct {
	Signals        []SkillSignal `json:"signals"`
	Next           string        `json:"next_cursor,omitempty"`
	CatalogVersion int64         `json:"catalog_version"`
}

type SkillSignalRead struct {
	ID         int64  `json:"id"`
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	AttemptID  string `json:"attempt_id"`
	RoleID     string `json:"role_id,omitempty"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Bytes      int64  `json:"bytes"`
	ObservedAt string `json:"observed_at"`
	Outcome    string `json:"outcome"`
	Uncertain  bool   `json:"effects_uncertain"`
}

type SkillSignalEvidence struct {
	Signal SkillSignal       `json:"signal"`
	Reads  []SkillSignalRead `json:"reads"`
	Next   string            `json:"next_cursor,omitempty"`
}

type SkillCollectionJob struct {
	OutcomeID    int64  `json:"outcome_id"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	StateVersion int64  `json:"state_version"`
	State        string `json:"state"`
	ReadCursor   int64  `json:"read_cursor"`
	Attempts     int    `json:"attempts"`
	ErrorCode    string `json:"error_code,omitempty"`
	RetryAfterNS int64  `json:"retry_after_ns,omitempty"`
}

type SkillCollectorStatus struct {
	Running   bool                 `json:"running"`
	Pending   int64                `json:"pending"`
	Completed int64                `json:"completed"`
	Failed    int64                `json:"failed"`
	Jobs      []SkillCollectionJob `json:"jobs"`
	// Jobs is the bounded unresolved head (maximum 100); counts cover all jobs.
	Truncated bool `json:"truncated"`
}

type SkillCollectionJobPage struct {
	Jobs []SkillCollectionJob `json:"jobs"`
	Next int64                `json:"next_outcome_id,omitempty"`
}
