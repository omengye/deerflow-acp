package harness

import (
	"context"
	"strings"
	"time"
)

// A capability name is a readable suggestion, never a source/path grant. Avoid
// opaque workflow hashes and numeric-only labels without inventing a fallback.
func ValidSkillCapabilityName(name string) bool {
	if len(name) > 56 || !ValidSkillName(name) {
		return false
	}
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		letters, hex := false, true
		for _, c := range part {
			letters = letters || c >= 'a' && c <= 'z'
			hex = hex && (c >= 'a' && c <= 'f' || c >= '0' && c <= '9')
		}
		if !letters || hex && len(part) >= 12 {
			return false
		}
	}
	return true
}

// SkillLearningOptions enables background procedural experience capture. It
// grants neither skill generation nor publication; those remain separate opts.
type SkillLearningOptions struct {
	Model SkillGenerationModelConfig
	// Consolidation is a separate paid-work opt-in. Nil keeps extraction-only
	// shadow mode; semantic grouping cannot grant generation or publication.
	Consolidation *SkillGenerationModelConfig
	// CrossSession permits workspace-private aggregation only for sessions that
	// separately consent to sharing. It never widens ordinary session histories.
	CrossSession bool
	// Matching is independently enabled and requires consolidation. It can
	// classify gaps in authorized skills, never generate or publish a change.
	Matching           *SkillGenerationModelConfig
	Budget             SkillMaintenanceBudget
	Idle               time.Duration
	MaxDailyModelCalls int64
	MaxDailyTokens     int64
}

type SkillLearningControl struct {
	SessionID       string `json:"session_id"`
	Contribute      bool   `json:"contribute"`
	WorkspaceShared bool   `json:"workspace_shared"`
	Epoch           int64  `json:"consent_epoch"`
}

type SkillLearningControlRequest struct {
	Contribute    bool  `json:"contribute"`
	ExpectedEpoch int64 `json:"expected_epoch"`
	// Nil preserves the current sharing choice; contribution opt-out clears it.
	WorkspaceShared *bool `json:"workspace_shared,omitempty"`
}

// Lineage links a new, empty foreground session to an existing task. This is
// host management authority, never a model- or single-session ACP capability.
type SkillLearningLineageRequest struct {
	SourceSessionID     string `json:"source_session_id"`
	SourceEpisodeID     string `json:"source_episode_id"`
	ExpectedEpoch       int64  `json:"expected_epoch"`
	ExpectedSourceEpoch int64  `json:"expected_source_epoch"`
}

type SkillLearningReference struct {
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	InputID     string    `json:"input_id,omitempty"`
	Sequence    int64     `json:"sequence,omitempty"`
	ToolCallID  string    `json:"tool_call_id,omitempty"`
	Version     int64     `json:"version,omitempty"`
	Kind        string    `json:"kind"`
	State       string    `json:"state,omitempty"`
	Text        string    `json:"text,omitempty"`
	ToolName    string    `json:"tool_name,omitempty"`
	FailureCode string    `json:"failure_code,omitempty"`
	Skill       *SkillRef `json:"skill,omitempty"`
	Path        string    `json:"path,omitempty"`
	Hash        string    `json:"sha256,omitempty"`
	Truncated   bool      `json:"truncated,omitempty"`
	RoleID      string    `json:"role_id,omitempty"`
}

type SkillLearningInput struct {
	EpisodeID  string                   `json:"episode_id"`
	References []SkillLearningReference `json:"references"`
}

type SkillExperienceCandidate struct {
	Kind          string   `json:"kind"`
	Goal          string   `json:"goal"`
	Applicability string   `json:"applicability"`
	Problem       string   `json:"problem,omitempty"`
	Resolution    string   `json:"resolution"`
	Steps         []string `json:"steps"`
	Verification  string   `json:"verification"`
	Outcome       string   `json:"outcome"`
	References    []string `json:"references"`
}

type SkillExperience struct {
	ID        string                   `json:"id"`
	SessionID string                   `json:"session_id"`
	EpisodeID string                   `json:"episode_id"`
	Revision  int64                    `json:"revision"`
	Origin    string                   `json:"origin"`
	Quality   string                   `json:"quality"`
	Candidate SkillExperienceCandidate `json:"experience"`
	CreatedAt time.Time                `json:"created_at"`
}

type SkillExperiencePage struct {
	Experiences []SkillExperience `json:"experiences"`
	Next        string            `json:"next_cursor,omitempty"`
}

// Source reads are host-only inspection of one experience's cited, redacted
// extraction view. They cannot grant generation, publication or quality votes.
type SkillLearningSourceRequest struct {
	Kind             string `json:"kind"`
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Scope            string `json:"scope"`
	ExperienceID     string `json:"experience_id"`
}

type SkillLearningSource struct {
	SessionID             string                     `json:"session_id"`
	Source                SkillLearningSourceRequest `json:"source"`
	SourceSessionID       string                     `json:"source_session_id"`
	EpisodeID             string                     `json:"episode_id"`
	ConsentEpoch          int64                      `json:"consent_epoch"`
	ExtractionID          string                     `json:"extraction_id"`
	InputSHA              string                     `json:"input_sha256"`
	ResultSHA             string                     `json:"result_sha256"`
	PolicyVersion         string                     `json:"policy_version"`
	ExternalContextPolicy string                     `json:"external_context_policy"`
	SourceState           string                     `json:"source_state"`
	Quality               string                     `json:"quality"`
	References            []SkillLearningReference   `json:"references"`
}

type SkillExperiencePattern struct {
	ID               string                      `json:"id"`
	SessionID        string                      `json:"session_id"`
	Revision         int64                       `json:"revision"`
	Origin           string                      `json:"origin"`
	Quality          string                      `json:"quality"`
	RepresentativeID string                      `json:"representative_id"`
	Candidate        SkillExperienceCandidate    `json:"experience"`
	ExperienceIDs    []string                    `json:"experience_ids"`
	EpisodeIDs       []string                    `json:"episode_ids"`
	SupportEpisodes  int                         `json:"support_episodes"`
	ObservedEpisodes int                         `json:"observed_episodes"`
	EvidenceState    string                      `json:"evidence_state"`
	SourceState      string                      `json:"source_state"`
	ConflictsWith    []string                    `json:"conflicts_with"`
	UpdatedAt        time.Time                   `json:"updated_at"`
	Review           *SkillLearningPatternReview `json:"review,omitempty"`
}

// Review controls suggestions, never skill quality or installed assets. Its
// version is independent of pattern revisions and consent epochs.
type SkillLearningPatternReview struct {
	PatternID            string `json:"pattern_id"`
	PatternRevision      int64  `json:"pattern_revision"`
	StateVersion         int64  `json:"state_version"`
	Ignored              bool   `json:"ignored"`
	AdjudicationRevision int64  `json:"adjudication_revision"`
	AdjudicationID       string `json:"adjudication_id"`
}

type SkillLearningPatternIgnoreRequest struct {
	PatternID               string `json:"pattern_id"`
	ExpectedPatternRevision int64  `json:"expected_pattern_revision"`
	ExpectedStateVersion    int64  `json:"expected_state_version"`
	Scope                   string `json:"scope"`
	RequestKey              string `json:"request_key"`
	Ignored                 bool   `json:"ignored"`
}

// Adjudication names one currently runtime-visible existing skill and exact
// head. It makes no model call, quality vote, new-skill reservation or publish.
type SkillLearningPatternAdjudicationRequest struct {
	PatternID               string      `json:"pattern_id"`
	ExpectedPatternRevision int64       `json:"expected_pattern_revision"`
	ExpectedStateVersion    int64       `json:"expected_state_version"`
	Scope                   string      `json:"scope"`
	RequestKey              string      `json:"request_key"`
	Target                  SkillTarget `json:"target"`
	ExpectedHead            SkillHead   `json:"expected_head"`
	OverlayBase             *SkillRef   `json:"overlay_base"`
	Relation                string      `json:"relation"`
	Reason                  string      `json:"reason"`
}

type SkillLearningPatternReviewResult struct {
	Review SkillLearningPatternReview `json:"review"`
	Match  *SkillExperienceMatch      `json:"match,omitempty"`
}

// A creation retry is explicit host authority for another attempt, not another
// independent episode or a refund of the automatic lifetime reservation.
type SkillLearningNewSkillRetryPreparationRequest struct {
	PatternID               string `json:"pattern_id"`
	ExpectedPatternRevision int64  `json:"expected_pattern_revision"`
	ExpectedStateVersion    int64  `json:"expected_state_version"`
	Scope                   string `json:"scope"`
	PreviousGenerationID    string `json:"previous_generation_id"`
}

type SkillLearningNewSkillRetryRequest struct {
	PatternID                      string      `json:"pattern_id"`
	ExpectedPatternRevision        int64       `json:"expected_pattern_revision"`
	ExpectedStateVersion           int64       `json:"expected_state_version"`
	Scope                          string      `json:"scope"`
	RequestKey                     string      `json:"request_key"`
	PreviousGenerationID           string      `json:"previous_generation_id"`
	ExpectedGenerationStateVersion int64       `json:"expected_generation_state_version"`
	PreviousProposalID             string      `json:"previous_proposal_id"`
	ExpectedProposalStateVersion   int64       `json:"expected_proposal_state_version"`
	Target                         SkillTarget `json:"target"`
	ExpectedHead                   SkillHead   `json:"expected_head"`
	OverlayBase                    *SkillRef   `json:"overlay_base"`
	EvidenceReservationID          string      `json:"evidence_reservation_id"`
	ExpectedRetryNumber            int         `json:"expected_retry_number"`
}

type SkillLearningNewSkillRetryReference struct {
	PreviousGenerationID  string `json:"previous_generation_id"`
	EvidenceReservationID string `json:"evidence_reservation_id"`
	RetryNumber           int    `json:"retry_number"`
}

type SkillLearningNewSkillRetryPreparation struct {
	Request                 SkillLearningNewSkillRetryRequest   `json:"request"`
	PreviousGenerationState string                              `json:"previous_generation_state"`
	PreviousProposalStatus  string                              `json:"previous_proposal_status"`
	Retry                   SkillLearningNewSkillRetryReference `json:"retry"`
	MaxRetries              int                                 `json:"max_retries"`
}

type SkillLearningNewSkillRetryResult struct {
	ID           string                              `json:"id"`
	Review       SkillLearningPatternReview          `json:"review"`
	GenerationID string                              `json:"generation_id"`
	DiscoveryID  string                              `json:"discovery_id"`
	Retry        SkillLearningNewSkillRetryReference `json:"retry"`
}

type SkillExperiencePatternPage struct {
	Patterns []SkillExperiencePattern `json:"patterns"`
	Next     string                   `json:"next_cursor,omitempty"`
}

// The host fixes the seed and shortlist. The model may compare semantics, but
// cannot rewrite recipes, add members, count successes or choose a skill.
type SkillExperienceConsolidationSeed struct {
	ID        string                   `json:"id"`
	EpisodeID string                   `json:"episode_id"`
	Candidate SkillExperienceCandidate `json:"experience"`
}

type SkillExperienceConsolidationChoice struct {
	ID            string                   `json:"id"`
	Revision      int64                    `json:"revision"`
	Candidate     SkillExperienceCandidate `json:"experience"`
	EvidenceState string                   `json:"evidence_state"`
}

type SkillExperienceConsolidationInput struct {
	Seed     SkillExperienceConsolidationSeed     `json:"seed"`
	Patterns []SkillExperienceConsolidationChoice `json:"patterns"`
}

type SkillExperienceConsolidationDecision struct {
	Relation  string `json:"relation"`
	PatternID string `json:"pattern_id"`
	Reason    string `json:"reason"`
}

type SkillExperienceMatchChoice struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	Body               string `json:"body"`
	ActuallyRead       bool   `json:"actually_read"`
	CurrentVersionRead bool   `json:"current_version_read"`
}

type SkillExperienceMatchInput struct {
	PatternID         string                       `json:"pattern_id"`
	Revision          int64                        `json:"revision"`
	Candidate         SkillExperienceCandidate     `json:"experience"`
	EvidenceState     string                       `json:"evidence_state"`
	InventoryComplete bool                         `json:"inventory_complete"`
	Skills            []SkillExperienceMatchChoice `json:"skills"`
}

type SkillExperienceMatchDecision struct {
	Relation      string `json:"relation"`
	SkillID       string `json:"skill_id"`
	Reason        string `json:"reason"`
	SuggestedName string `json:"suggested_name,omitempty"`
}

type SkillExperienceMatch struct {
	ID              string                      `json:"id"`
	SessionID       string                      `json:"session_id"`
	Origin          string                      `json:"origin"`
	Quality         string                      `json:"quality"`
	PatternID       string                      `json:"pattern_id"`
	PatternRevision int64                       `json:"pattern_revision"`
	Relation        string                      `json:"relation"`
	Reason          string                      `json:"reason"`
	SuggestedName   string                      `json:"suggested_name,omitempty"`
	Target          *SkillTarget                `json:"target,omitempty"`
	Head            *SkillHead                  `json:"head,omitempty"`
	OverlayBase     *SkillRef                   `json:"overlay_base,omitempty"`
	ActualReads     []SkillRef                  `json:"actual_reads"`
	EvidenceState   string                      `json:"evidence_state"`
	SourceState     string                      `json:"source_state"`
	CreatedAt       time.Time                   `json:"created_at"`
	Review          *SkillLearningPatternReview `json:"review,omitempty"`
}

type SkillExperienceMatchPage struct {
	Matches []SkillExperienceMatch `json:"matches"`
	Next    string                 `json:"next_cursor,omitempty"`
}

type SkillLearningJob struct {
	ID              string    `json:"id"`
	EpisodeID       string    `json:"episode_id"`
	PatternID       string    `json:"pattern_id,omitempty"`
	PatternRevision int64     `json:"pattern_revision,omitempty"`
	Kind            string    `json:"kind"`
	State           string    `json:"state"`
	StateVersion    int64     `json:"state_version"`
	Attempts        int       `json:"attempts"`
	ErrorCode       string    `json:"error_code,omitempty"`
	Deadline        time.Time `json:"deadline,omitempty"`
}

type SkillLearningJobPage struct {
	Jobs []SkillLearningJob `json:"jobs"`
	Next string             `json:"next_cursor,omitempty"`
}

type SkillLearningStatus struct {
	Version                  string `json:"version"`
	Enabled                  bool   `json:"enabled"`
	Mode                     string `json:"mode"`
	IdleSeconds              int64  `json:"idle_seconds"`
	Pending                  int64  `json:"pending_inputs"`
	Experiences              int64  `json:"experiences"`
	ConsolidationEnabled     bool   `json:"consolidation_enabled"`
	Patterns                 int64  `json:"patterns"`
	MatchingEnabled          bool   `json:"matching_enabled"`
	CrossSessionEnabled      bool   `json:"cross_session_enabled"`
	Matches                  int64  `json:"matches"`
	ProposalDiscoveryEnabled bool   `json:"proposal_discovery_enabled"`
	AutoPatchEnabled         bool   `json:"auto_patch_enabled"`
}

// Extractors receive only a redacted, bounded view. The host verifies every
// returned reference; confidence and text cannot become operator feedback.
type SkillExperienceExtractor interface {
	Identity() string
	Extract(context.Context, SkillLearningInput) ([]SkillExperienceCandidate, error)
}

type SkillExperienceConsolidator interface {
	Identity() string
	Consolidate(context.Context, SkillExperienceConsolidationInput) (SkillExperienceConsolidationDecision, error)
}

type SkillExperienceMatcher interface {
	Identity() string
	Match(context.Context, SkillExperienceMatchInput) (SkillExperienceMatchDecision, error)
}
