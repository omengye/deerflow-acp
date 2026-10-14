package harness

import "time"

// SkillDiscoveryOptions opts into explicitly selected automatic proposal
// sources. It does not assess skill causality or publish skills.
type SkillDiscoveryOptions struct {
	DisableOperatorCorrections bool `json:"disable_operator_corrections,omitempty"`
	MissingReportsEnabled      bool `json:"missing_reports_enabled,omitempty"`
	SuccessfulWorkflowsEnabled bool `json:"successful_workflows_enabled,omitempty"`
	ExperiencePatternsEnabled  bool `json:"experience_patterns_enabled,omitempty"`
	ExperienceNewSkillsEnabled bool `json:"experience_new_skills_enabled,omitempty"`
	// Review drafts are generated automatically from one observed episode,
	// but never qualify for automatic publication.
	ExperienceReviewDraftsEnabled bool   `json:"experience_review_drafts_enabled,omitempty"`
	NewSkillSourceID              string `json:"new_skill_source_id,omitempty"`
	RepeatThreshold               int    `json:"repeat_threshold"`
	RepeatWindowDays              int    `json:"repeat_window_days"`
	CooldownHours                 int    `json:"cooldown_hours"`
	MaxDailyProposals             int    `json:"max_daily_proposals"`
	MaxPendingProposals           int    `json:"max_pending_proposals"`
}

type SkillDiscoveryStatus struct {
	Version           string                 `json:"version"`
	Enabled           bool                   `json:"enabled"`
	Running           bool                   `json:"running"`
	Triggers          []string               `json:"triggers"`
	Options           *SkillDiscoveryOptions `json:"options,omitempty"`
	Sources           []SkillTarget          `json:"sources"`
	Today             string                 `json:"today"`
	DailyReservations int                    `json:"daily_reservations"`
	Pending           int                    `json:"pending"`
	FailedSignals     int                    `json:"failed_signals"`
	LastErrorCode     string                 `json:"last_error_code,omitempty"`
}

// Evidence records reference immutable observed revisions. Summaries are
// resolved from live, sanitized feedback or learning sources before use.
type SkillDiscoveryEvidence struct {
	SignalID           string                             `json:"signal_id"`
	RootID             string                             `json:"root_id"`
	EpisodeKey         string                             `json:"episode_key,omitempty"`
	SignalStateVersion int64                              `json:"signal_state_version"`
	FeedbackRevision   int64                              `json:"feedback_revision"`
	ReadIDs            []int64                            `json:"read_ids"`
	MissingReportID    string                             `json:"missing_report_id,omitempty"`
	ReportRevision     int64                              `json:"report_revision,omitempty"`
	WorkflowDigest     string                             `json:"workflow_digest,omitempty"`
	Receipts           []SkillWorkflowReceipt             `json:"receipts,omitempty"`
	Experience         *SkillExperienceDiscoveryReference `json:"experience,omitempty"`
}

// These references pin host-verified learning revisions, not quality votes.
// Model summaries are rebuilt under the caller's transaction before use.
type SkillExperienceDiscoveryReference struct {
	MatchID         string `json:"match_id"`
	PatternID       string `json:"pattern_id"`
	PatternRevision int64  `json:"pattern_revision"`
	MatchInputSHA   string `json:"match_input_sha256"`
	MatchResultSHA  string `json:"match_result_sha256"`
	PatternSHA      string `json:"pattern_sha256"`
	PolicyIdentity  string `json:"policy_identity"`
	EpisodeID       string `json:"episode_id"`
	IndependenceKey string `json:"independence_key"`
}

type SkillWorkflowReceipt struct {
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	Version    int64  `json:"version"`
}

type SkillDiscovery struct {
	ID                 string                               `json:"id"`
	Target             SkillTarget                          `json:"target"`
	Base               SkillHead                            `json:"base"`
	GenerationID       string                               `json:"generation_id"`
	CreatedAt          time.Time                            `json:"created_at"`
	Attribution        string                               `json:"attribution"`
	Kind               string                               `json:"kind"`
	Evidence           []SkillDiscoveryEvidence             `json:"evidence"`
	State              string                               `json:"state"`
	ProposalID         string                               `json:"proposal_id,omitempty"`
	HostAdjudicated    bool                                 `json:"host_adjudicated,omitempty"`
	ReviewDraft        bool                                 `json:"review_draft,omitempty"`
	CreationRetry      *SkillLearningNewSkillRetryReference `json:"creation_retry,omitempty"`
	PreviousProposalID string                               `json:"previous_proposal_id,omitempty"`
}

type SkillDiscoveryPage struct {
	Discoveries []SkillDiscovery `json:"discoveries"`
	Next        string           `json:"next_cursor,omitempty"`
}
