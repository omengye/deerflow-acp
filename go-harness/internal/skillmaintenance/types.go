// Package skillmaintenance owns the separate, durable budget for skill
// generation and evaluation. It never creates user runs or invokes tools.
package skillmaintenance

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var (
	ErrInvalid               = errors.New("invalid skill maintenance request")
	ErrConflict              = errors.New("skill maintenance state changed")
	ErrBudget                = errors.New("skill maintenance daily budget exhausted")
	ErrClock                 = errors.New("skill maintenance clock moved backwards")
	ErrSourceActive          = errors.New("skill maintenance source is active")
	ErrMatchInventoryChanged = errors.New("skill matching inventory changed")
)

// Policy must be explicit. Zero never means unlimited. UTC days share one
// budget across all sources and stages in the database. Jobs pin their policy;
// a later stricter host policy also applies at admission.
type Policy = harness.SkillMaintenanceBudget

func validPolicy(p Policy) bool {
	return p.MaxDailyModelCalls >= 1 && p.MaxDailyModelCalls <= 100000 && p.MaxDailyTokens >= 1 && p.MaxDailyTokens <= 1000000000 && p.MaxJobSeconds >= 1 && p.MaxJobSeconds <= 3600 && p.Workers >= 1 && p.Workers <= 16
}

// Reference and InputDigest identify a producer-owned immutable input record.
// The queue does not copy conversation text, credentials or provider errors.
type Request struct {
	Kind           JobKind   `json:"job_kind,omitempty"`
	OwnerKind      OwnerKind `json:"owner_kind,omitempty"`
	OwnerKey       string    `json:"owner_key,omitempty"`
	SourceIdentity string    `json:"source_identity"`
	Name           string    `json:"name"`
	Key            string    `json:"request_key"`
	Reference      string    `json:"reference"`
	InputDigest    string    `json:"input_digest"`
}

type JobKind string

const (
	LegacyJob             JobKind = "legacy"
	SkillGeneration       JobKind = "skill_generation"
	SkillBehavior         JobKind = "skill_behavior"
	ExperienceExtract     JobKind = "experience_extract"
	ExperienceConsolidate JobKind = "experience_consolidate"
	SkillMatch            JobKind = "skill_match"
)

type OwnerKind string

const (
	SkillOwner     OwnerKind = "skill"
	WorkspaceOwner OwnerKind = "workspace"
	UserOwner      OwnerKind = "user"
)

func validKind(kind JobKind) bool {
	switch kind {
	case SkillGeneration, SkillBehavior, ExperienceExtract, ExperienceConsolidate, SkillMatch:
		return true
	default:
		return false
	}
}

// SkillOwnerKey is a namespace, not a source or publication grant.
func SkillOwnerKey(source, name string) string { return hash([]string{source, name}) }

func normalizeRequest(r Request, stored bool) (Request, bool) {
	if r.Kind == "" {
		r.Kind = SkillGeneration
	}
	if !bounded(r.Key, 256) || !bounded(r.Reference, 256) || !digest(r.InputDigest) {
		return r, false
	}
	if r.Kind == SkillGeneration || r.Kind == SkillBehavior || stored && r.Kind == LegacyJob {
		if r.OwnerKind == "" {
			r.OwnerKind = SkillOwner
		}
		if r.OwnerKey == "" {
			r.OwnerKey = SkillOwnerKey(r.SourceIdentity, r.Name)
		}
		return r, r.OwnerKind == SkillOwner && digest(r.SourceIdentity) && harness.ValidSkillName(r.Name) && r.OwnerKey == SkillOwnerKey(r.SourceIdentity, r.Name)
	}
	return r, validKind(r.Kind) && r.SourceIdentity == "" && r.Name == "" &&
		(r.OwnerKind == WorkspaceOwner || r.OwnerKind == UserOwner) && digest(r.OwnerKey)
}

func requestStorageKey(r Request) string {
	if r.OwnerKind == SkillOwner {
		return r.Key
	}
	// Keep the legacy target constraint without inventing a target. Unbound
	// jobs store empty source/name and a scope-qualified request key.
	return "learning/" + hash([]string{string(r.Kind), string(r.OwnerKind), r.OwnerKey, r.Key})
}

type Job struct {
	ID           string    `json:"id"`
	Request      Request   `json:"request"`
	Policy       Policy    `json:"policy"`
	State        string    `json:"state"`
	StateVersion int64     `json:"state_version"`
	Attempts     int       `json:"attempts"`
	Deadline     time.Time `json:"deadline,omitempty"`
	RetryAt      time.Time `json:"retry_at,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
}

type Claim struct {
	Job   Job
	token string
}

type ModelCall struct {
	Stage           string
	ModelIdentity   string // digest of provider/model/options, never credentials
	InputDigest     string
	InputTokens     int64 // conservative estimate before I/O
	MaxOutputTokens int64
}

type Usage struct {
	InputTokens, OutputTokens int64
	Known                     bool // only a complete, valid provider usage record can release credit
}

type Receipt = harness.SkillMaintenanceReceipt

type DayUsage struct {
	Day           string `json:"day"`
	ModelCalls    int64  `json:"model_calls"`
	ChargedTokens int64  `json:"charged_tokens"`
}

// Broker is installed only by a trusted maintenance worker. A provider call
// requires a committed reservation; settlement errors must stop further calls.
type Broker interface {
	Reserve(context.Context, ModelCall) (Receipt, error)
	Settle(context.Context, Receipt, Usage) error
}

type brokerKey struct{}

func WithBroker(ctx context.Context, broker Broker) context.Context {
	return context.WithValue(ctx, brokerKey{}, broker)
}

func BrokerFromContext(ctx context.Context) (Broker, bool) {
	b, ok := ctx.Value(brokerKey{}).(Broker)
	return b, ok && b != nil
}
