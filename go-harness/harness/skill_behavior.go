package harness

import (
	"context"
	"encoding/json"
	"time"
)

// Behavior options are loaded and authorized by the host. Candidates cannot
// supply or modify the fixture suite, assertions or evaluation model.
type SkillBehaviorOptions struct{ Suites []SkillBehaviorSuite }

type SkillBehaviorModelConfig struct {
	Route                                                       ModelRoute
	Timeout                                                     time.Duration
	MaxInputBytes, MaxOutputTokens, MaxModelCalls, MaxToolCalls int
}

type SkillBehaviorSuite struct {
	Version  string              `json:"version"`
	ID       string              `json:"id"`
	SourceID string              `json:"source_id"`
	Name     string              `json:"name"`
	Cases    []SkillBehaviorCase `json:"cases"`
}

type SkillBehaviorCase struct {
	ID          string                   `json:"id"`
	Kind        string                   `json:"kind"`
	Prompt      string                   `json:"prompt"`
	Files       map[string]string        `json:"files"`
	Fixtures    []SkillBehaviorFixture   `json:"fixtures"`
	Assertions  []SkillBehaviorAssertion `json:"assertions"`
	Repetitions int                      `json:"repetitions"`
}

type SkillBehaviorFixture struct {
	Name        string                            `json:"name"`
	Description string                            `json:"description"`
	Parameters  map[string]SkillBehaviorParameter `json:"parameters"`
	Exchanges   []SkillBehaviorExchange           `json:"exchanges"`
}

type SkillBehaviorParameter struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type SkillBehaviorExchange struct {
	Arguments json.RawMessage `json:"arguments"`
	Output    string          `json:"output"`
}

type SkillBehaviorAssertion struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Expected string `json:"expected,omitempty"`
	Count    int    `json:"count,omitempty"`
}

// Evaluate executes exactly one fixed variant/case/repetition, with a fresh
// local fixture workspace and independent model context. All model calls use
// the maintenance broker carried in ctx; arbitrary scripts are not executed.
type SkillBehaviorEvaluator interface {
	Identity() string
	Evaluate(context.Context, SkillBehaviorInput) (SkillBehaviorResult, error)
}

type SkillBehaviorInput struct {
	EvaluationID, CaseID, Variant, Version string
	Repetition                             int
	Files                                  map[string][]byte
	Case                                   SkillBehaviorCase
}

type SkillBehaviorCheck struct {
	ID           string `json:"id"`
	Passed       bool   `json:"passed"`
	ActualDigest string `json:"actual_digest"`
}

type SkillBehaviorResult struct {
	CaseID         string               `json:"case_id"`
	Variant        string               `json:"variant"`
	Version        string               `json:"version"`
	Repetition     int                  `json:"repetition"`
	Passed         bool                 `json:"passed"`
	StopReason     string               `json:"stop_reason"`
	ErrorCode      string               `json:"error_code,omitempty"`
	OutputDigest   string               `json:"output_digest"`
	TraceDigest    string               `json:"trace_digest"`
	Checks         []SkillBehaviorCheck `json:"checks"`
	ModelCalls     int64                `json:"model_calls"`
	ToolCalls      int64                `json:"tool_calls"`
	InputTokens    int64                `json:"input_tokens"`
	OutputTokens   int64                `json:"output_tokens"`
	UsageEstimated bool                 `json:"usage_estimated"`
	ElapsedMillis  int64                `json:"elapsed_millis"`
}

// The compact proposal report references separately persisted per-case results.
// It is composed by the evolution host, never accepted from content reviewers.
type SkillBehaviorReport struct {
	EvaluationID      string                    `json:"evaluation_id"`
	EvaluatorIdentity string                    `json:"evaluator_identity"`
	SuiteID           string                    `json:"suite_id"`
	SuiteDigest       string                    `json:"suite_digest"`
	RuleVersion       string                    `json:"rule_version"`
	BaseVersion       string                    `json:"base_version"`
	CandidateVersion  string                    `json:"candidate_version"`
	State             string                    `json:"state"`
	TargetedCases     int                       `json:"targeted_cases"`
	RegressionCases   int                       `json:"regression_cases"`
	Regressions       int                       `json:"regressions"`
	Improvements      int                       `json:"improvements"`
	Results           []SkillBehaviorComparison `json:"results"`
	ModelCalls        int64                     `json:"model_calls"`
	ToolCalls         int64                     `json:"tool_calls"`
	InputTokens       int64                     `json:"input_tokens"`
	OutputTokens      int64                     `json:"output_tokens"`
	UsageEstimated    bool                      `json:"usage_estimated"`
	ElapsedMillis     int64                     `json:"elapsed_millis"`
}

type SkillBehaviorComparison struct {
	CaseID                string `json:"case_id"`
	Kind                  string `json:"kind"`
	Repetition            int    `json:"repetition"`
	BasePassed            bool   `json:"base_passed"`
	CandidatePassed       bool   `json:"candidate_passed"`
	BaseOutputDigest      string `json:"base_output_digest"`
	CandidateOutputDigest string `json:"candidate_output_digest"`
	BaseErrorCode         string `json:"base_error_code,omitempty"`
	CandidateErrorCode    string `json:"candidate_error_code,omitempty"`
}

type SkillBehaviorEvaluation struct {
	ID                string                `json:"id"`
	ProposalID        string                `json:"proposal_id"`
	JobID             string                `json:"job_id"`
	Target            SkillTarget           `json:"target"`
	Base              SkillHead             `json:"base"`
	CandidateVersion  string                `json:"candidate_version"`
	SuiteID           string                `json:"suite_id,omitempty"`
	SuiteDigest       string                `json:"suite_digest,omitempty"`
	EvaluatorIdentity string                `json:"evaluator_identity"`
	State             string                `json:"state"`
	StateVersion      int64                 `json:"state_version"`
	Attempts          int                   `json:"attempts"`
	ErrorCode         string                `json:"error_code,omitempty"`
	Deadline          time.Time             `json:"deadline,omitempty"`
	RetryAt           time.Time             `json:"retry_at,omitempty"`
	ContentReviewed   bool                  `json:"content_reviewed"`
	Results           []SkillBehaviorResult `json:"results"`
	Report            *SkillBehaviorReport  `json:"report,omitempty"`
}
