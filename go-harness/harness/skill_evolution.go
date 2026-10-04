// The operator skill APIs manage durable candidates, reviews and publication
// history. It never executes candidate scripts or writes a mutable source tree.
package harness

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSkillConflict   = errors.New("skill proposal or publication changed")
	ErrSkillValidation = errors.New("skill candidate validation is incomplete or failed")
)

// SkillTarget binds a source name to its configured root and scope identity.
// Model tools must additionally authorize it against the host session policy.
type SkillTarget struct {
	SourceID       string     `json:"source_id"`
	SourceIdentity string     `json:"source_identity"`
	Name           string     `json:"name"`
	Scope          SkillScope `json:"scope"`
	Workspace      string     `json:"workspace,omitempty"`
}

// SkillHead detects content changes and intervening rollbacks to the same
// content. Compare the complete value when proposing or rolling back a skill.
type SkillHead struct {
	Version      string `json:"version"`
	StateVersion int64  `json:"state_version"`
	Exists       bool   `json:"exists"`
	Enabled      bool   `json:"enabled"`
	Managed      bool   `json:"managed"`
}

type SkillProposalPage struct {
	Proposals      []SkillProposal `json:"proposals"`
	Next           string          `json:"next_cursor,omitempty"`
	CatalogVersion int64           `json:"catalog_version"`
}

type SkillHistoryPage struct {
	Publications   []SkillPublication `json:"revisions"`
	Next           string             `json:"next_cursor,omitempty"`
	CatalogVersion int64              `json:"catalog_version"`
}

// SkillQuery is an operator query over currently configured source identities.
// Empty source/name selects all configured targets. Cursors bind these filters.
type SkillQuery struct {
	SourceID, Name, Status, Cursor string
	Limit                          int
}

type SkillTrigger struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Summary  string `json:"summary"`
}

type SkillProposal struct {
	ID                string                `json:"id"`
	Status            string                `json:"status"`
	StateVersion      int64                 `json:"state_version"`
	Action            string                `json:"action"`
	SkillName         string                `json:"skill_name"`
	Target            SkillTarget           `json:"target"`
	Base              SkillHead             `json:"base"`
	OverlayBase       *SkillRef             `json:"overlay_base,omitempty"`
	CandidateVersion  string                `json:"candidate_sha256,omitempty"`
	Reason            string                `json:"reason"`
	Trigger           SkillTrigger          `json:"trigger"`
	Origin            string                `json:"origin"`
	Author            string                `json:"author"`
	ChangedFiles      []string              `json:"changed_files"`
	Diff              string                `json:"diff,omitempty"`
	Evaluation        SkillValidationReport `json:"evaluation"`
	EvaluationDigest  string                `json:"evaluation_sha256,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
	ReviewedAt        *time.Time            `json:"reviewed_at,omitempty"`
	ReviewNote        string                `json:"review_note,omitempty"`
	ReviewedBy        string                `json:"reviewed_by,omitempty"`
	PublishedRevision int64                 `json:"published_revision,omitempty"`
	BehaviorJobID     string                `json:"behavior_job_id,omitempty"`
	Error             string                `json:"error,omitempty"`
}

// SkillValidationReport is produced by the host's configured validator, never by the model
// requesting the mutation. A missing/unavailable security check blocks review.
type SkillValidationReport struct {
	ValidatorVersion string               `json:"validator_version"`
	BaseVersion      string               `json:"base_version"`
	CandidateVersion string               `json:"candidate_version"`
	Security         string               `json:"security"`
	Behavior         string               `json:"behavior"`
	IsolatedScripts  bool                 `json:"isolated_scripts"`
	Evidence         []string             `json:"evidence,omitempty"`
	ModelChecks      []SkillModelCheck    `json:"model_checks,omitempty"`
	BehaviorReport   *SkillBehaviorReport `json:"behavior_report,omitempty"`
	BehaviorDigest   string               `json:"behavior_digest,omitempty"`
}

// Model checks are content review evidence. They do not represent a behavioral
// test run or permission to execute candidate scripts.
type SkillModelCheck struct {
	Stage          string `json:"stage"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Passed         bool   `json:"passed"`
	Reason         string `json:"reason"`
	InputHash      string `json:"input_hash"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	UsageEstimated bool   `json:"usage_estimated"`
}

// SkillModelReviewConfig creates two independent, tool-free model calls for
// manual review. The host must explicitly configure both routes. This is not
// the discovery-worker budget, and does not enable automatic publication.
type SkillModelReviewConfig struct {
	Moderation, Evaluation         ModelRoute
	Timeout                        time.Duration
	MaxInputBytes, MaxOutputTokens int
}

type SkillValidationInput struct {
	Proposal       SkillProposal
	BaseFiles      map[string][]byte
	CandidateFiles map[string][]byte
}

type SkillValidator interface {
	Validate(context.Context, SkillValidationInput) (SkillValidationReport, error)
}

// Supply Files for a complete create/edit candidate, or use Content for a
// SKILL.md create/edit while preserving existing support files. Patch is an
// exact text replacement in SKILL.md. Support-file mutations never modify the
// source directory and cannot replace SKILL.md. Nil differs from empty text.
type SkillProposalRequest struct {
	SourceID, Name, RequestKey, Action, Reason, Author, Origin string
	Base                                                       SkillHead
	Files                                                      map[string][]byte
	Trigger                                                    SkillTrigger
	Path                                                       string
	Content, Find, Replace                                     *string
	ExpectedCount                                              *int
}

type SkillPublication struct {
	Action         string      `json:"action"`
	Name           string      `json:"name"`
	Target         SkillTarget `json:"target"`
	Revision       int64       `json:"version"`
	ContentVersion string      `json:"sha256,omitempty"`
	Enabled        bool        `json:"enabled"`
	Deleted        bool        `json:"deleted"`
	Previous       SkillHead   `json:"previous"`
	Head           SkillHead   `json:"head"`
	OverlayBase    *SkillRef   `json:"overlay_base,omitempty"`
	ProposalID     string      `json:"source_proposal_id,omitempty"`
	Actor          string      `json:"actor"`
	Note           string      `json:"note,omitempty"`
	RollbackOf     int64       `json:"rollback_of,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
}

type SkillReview struct {
	ExpectedStateVersion int64
	Actor, Note          string
}

type SkillRollback struct {
	SourceID, Name, RequestKey, Actor, Note string
	Revision                                int64
	Expected                                SkillHead
}
