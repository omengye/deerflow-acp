package harness

import "time"

// SkillAutoPatchOptions opts into bounded automatic publication of generated,
// reviewed SKILL.md patches. New skills and supporting files retain review.
type SkillAutoPatchOptions struct {
	MaxChangedLines          int `json:"max_changed_lines" yaml:"max_changed_lines"`
	MaxDailyPublications     int `json:"max_daily_publications" yaml:"max_daily_publications"`
	ObservationHours         int `json:"observation_hours" yaml:"observation_hours"`
	MinObservations          int `json:"min_observations" yaml:"min_observations"`
	RollbackFailureThreshold int `json:"rollback_failure_threshold" yaml:"rollback_failure_threshold"`
}

type SkillAutoPatchStatus struct {
	Enabled        bool                   `json:"enabled"`
	Running        bool                   `json:"running"`
	Options        *SkillAutoPatchOptions `json:"options,omitempty"`
	Today          string                 `json:"today"`
	PublishedToday int64                  `json:"published_today"`
	Observing      int64                  `json:"observing"`
	LastErrorCode  string                 `json:"last_error_code,omitempty"`
}

// Counts are independent root/version assessments during the observation
// period. Unknown, external, denied, cancelled and uncertain outcomes are
// ignored. Explicit operator confirmation is required for quality attribution.
type SkillAutoPatchObservation struct {
	ProposalID          string                `json:"proposal_id"`
	Target              SkillTarget           `json:"target"`
	PublicationRevision int64                 `json:"publication_revision"`
	BaselineRevision    int64                 `json:"baseline_revision"`
	Head                SkillHead             `json:"head"`
	Options             SkillAutoPatchOptions `json:"options"`
	Status              string                `json:"status"`
	StartedAt           time.Time             `json:"started_at"`
	Deadline            time.Time             `json:"deadline"`
	UpdatedAt           time.Time             `json:"updated_at"`
	Successes           int                   `json:"successes"`
	Failures            int                   `json:"failures"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	Ignored             int                   `json:"ignored"`
	RollbackRevision    int64                 `json:"rollback_revision,omitempty"`
	Reason              string                `json:"reason,omitempty"`
}

type SkillAutoPatchPage struct {
	Observations []SkillAutoPatchObservation `json:"observations"`
	Next         string                      `json:"next_cursor,omitempty"`
}
