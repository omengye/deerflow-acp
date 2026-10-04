package harness

import (
	"context"
	"time"
)

type SkillMaintenanceBudget struct {
	MaxDailyModelCalls int64 `json:"max_daily_model_calls"`
	MaxDailyTokens     int64 `json:"max_daily_tokens"`
	MaxJobSeconds      int64 `json:"max_job_seconds"`
	Workers            int   `json:"workers"`
}

type SkillGenerationModelConfig struct {
	Route           ModelRoute
	Timeout         time.Duration
	MaxInputBytes   int
	MaxOutputTokens int
}

// SkillGenerationOptions explicitly enables operator-requested, durable
// generation. It does not enable discovery or automatic publication.
type SkillGenerationOptions struct {
	Model                SkillGenerationModelConfig
	Budget               SkillMaintenanceBudget
	Sources              []string
	AllowBuiltinOverlays bool
}

type SkillGenerationRequest struct {
	SourceID   string    `json:"source_id"`
	Name       string    `json:"name"`
	RequestKey string    `json:"request_key"`
	Reason     string    `json:"reason"`
	Author     string    `json:"author"`
	Base       SkillHead `json:"base"`
}

type SkillGenerationJob struct {
	ID               string      `json:"id"`
	DiscoveryID      string      `json:"discovery_id,omitempty"`
	Target           SkillTarget `json:"target"`
	Base             SkillHead   `json:"base"`
	State            string      `json:"state"`
	Phase            string      `json:"phase"`
	StateVersion     int64       `json:"state_version"`
	Attempts         int         `json:"attempts"`
	Deadline         time.Time   `json:"deadline,omitempty"`
	RetryAt          time.Time   `json:"retry_at,omitempty"`
	ErrorCode        string      `json:"error_code,omitempty"`
	ProposalID       string      `json:"proposal_id,omitempty"`
	CandidateVersion string      `json:"candidate_sha256,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
}

type SkillGenerationPage struct {
	Jobs []SkillGenerationJob `json:"jobs"`
	Next string               `json:"next_cursor,omitempty"`
}

// Status describes the running host's grants and limits, not a config draft or
// an estimate of remaining quota. Source targets have an empty Name.
type SkillGenerationStatus struct {
	Version string                  `json:"version"`
	Enabled bool                    `json:"enabled"`
	Sources []SkillTarget           `json:"sources"`
	Budget  *SkillMaintenanceBudget `json:"budget,omitempty"`
}

// Preparation is a read-only observation, including absent targets. Enqueue
// must still verify the observed head and current source grant transactionally.
type SkillGenerationPreparation struct {
	Target      SkillTarget `json:"target"`
	Base        SkillHead   `json:"base"`
	OverlayBase *SkillRef   `json:"overlay_base,omitempty"`
}

type SkillMaintenanceReceipt struct {
	Sequence       int64  `json:"sequence"`
	ID             string `json:"id"`
	JobID          string `json:"job_id"`
	Attempt        int    `json:"attempt"`
	Day            string `json:"day"`
	Stage          string `json:"stage"`
	ModelIdentity  string `json:"model_identity"`
	InputDigest    string `json:"input_digest"`
	ReservedTokens int64  `json:"reserved_tokens"`
	ChargedTokens  int64  `json:"charged_tokens"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	State          string `json:"state"`
	Estimated      bool   `json:"estimated"`
}

// These host-side types contain no publication or filesystem authority.
type SkillGenerationInput struct {
	Target    SkillTarget
	Base      SkillHead
	Reason    string
	BaseFiles map[string][]byte
	Evidence  []SkillGenerationEvidence
}

type SkillGenerationEvidence struct {
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type SkillGeneratedCandidate struct {
	Reason string
	Files  map[string][]byte
}

type SkillCandidateGenerator interface {
	Identity() string
	Generate(context.Context, SkillGenerationInput) (SkillGeneratedCandidate, error)
}

// Generation pins the review implementation as well as the model generator.
// Manual validators need not implement this additional contract.
type SkillVersionedValidator interface {
	SkillValidator
	Identity() string
}
