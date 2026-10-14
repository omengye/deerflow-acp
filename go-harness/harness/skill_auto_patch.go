package harness

import "time"

// SkillAutoPatchOptions opts into bounded automatic publication of generated,
// reviewed SKILL.md patches. New skills and supporting files retain review.
type SkillAutoPatchOptions struct {
	// Learning candidates need a separate grant; existing auto_patch does not
	// authorize them. Description edits additionally require trusted v2 trigger
	// comparisons; new skills and other metadata changes still require review.
	ExperiencePatternsEnabled bool `json:"experience_patterns_enabled" yaml:"experience_patterns_enabled"`
	MaxChangedLines           int  `json:"max_changed_lines" yaml:"max_changed_lines"`
	MaxDailyPublications      int  `json:"max_daily_publications" yaml:"max_daily_publications"`
	ObservationHours          int  `json:"observation_hours" yaml:"observation_hours"`
	MinObservations           int  `json:"min_observations" yaml:"min_observations"`
	RollbackFailureThreshold  int  `json:"rollback_failure_threshold" yaml:"rollback_failure_threshold"`
}

type SkillAutoPatchStatus struct {
	Enabled        bool                     `json:"enabled"`
	Running        bool                     `json:"running"`
	Options        *SkillAutoPatchOptions   `json:"options,omitempty"`
	Today          string                   `json:"today"`
	PublishedToday int64                    `json:"published_today"`
	Observing      int64                    `json:"observing"`
	LastErrorCode  string                   `json:"last_error_code,omitempty"`
	Readiness      *SkillAutoPatchReadiness `json:"readiness,omitempty"`
}

// Readiness is a host observation, never a grant to publish a candidate.
type SkillAutoPatchReadiness struct {
	State                  string   `json:"state"`
	CatalogVersion         int64    `json:"catalog_version"`
	PolicyIdentity         string   `json:"policy_identity"`
	Installed              int64    `json:"installed"`
	EligibleTargets        int64    `json:"eligible_targets"`
	MissingSuite           int64    `json:"missing_suite"`
	PendingReview          int64    `json:"pending_review"`
	NeedsAttention         int64    `json:"needs_attention"`
	WaitingCandidates      int64    `json:"waiting_candidates"`
	NewSkillsRequireReview bool     `json:"new_skills_require_review"`
	Reasons                []string `json:"reasons"`
}

// Counts are independent root/version assessments during the observation
// period. Unknown, external, denied, cancelled and uncertain outcomes are
// ignored. Explicit operator confirmation is required for quality attribution.
type SkillAutoPatchObservation struct {
	ProposalID          string                        `json:"proposal_id"`
	Target              SkillTarget                   `json:"target"`
	PublicationRevision int64                         `json:"publication_revision"`
	BaselineRevision    int64                         `json:"baseline_revision"`
	Head                SkillHead                     `json:"head"`
	Options             SkillAutoPatchOptions         `json:"options"`
	Status              string                        `json:"status"`
	StartedAt           time.Time                     `json:"started_at"`
	Deadline            time.Time                     `json:"deadline"`
	UpdatedAt           time.Time                     `json:"updated_at"`
	Successes           int                           `json:"successes"`
	Failures            int                           `json:"failures"`
	ConsecutiveFailures int                           `json:"consecutive_failures"`
	Ignored             int                           `json:"ignored"`
	RollbackRevision    int64                         `json:"rollback_revision,omitempty"`
	Reason              string                        `json:"reason,omitempty"`
	LearningSource      *SkillAutoPatchLearningSource `json:"learning_source,omitempty"`
}

// Publication-time references contain no conversation or procedure text. They
// link observation/rollback to the admitted source, not to a quality judgment.
type SkillAutoPatchLearningSource struct {
	GenerationID    string `json:"generation_id"`
	DiscoveryID     string `json:"discovery_id"`
	DiscoverySHA    string `json:"discovery_sha256"`
	PatternID       string `json:"pattern_id"`
	PatternRevision int64  `json:"pattern_revision"`
	MatchID         string `json:"match_id"`
	PolicyIdentity  string `json:"policy_identity"`
	EvidenceSHA     string `json:"evidence_sha256"`
	// valid_at_publication is historical, not ongoing source authorization.
	SourceState string `json:"source_state"`
}

type SkillAutoPatchPage struct {
	Observations []SkillAutoPatchObservation `json:"observations"`
	Next         string                      `json:"next_cursor,omitempty"`
}
