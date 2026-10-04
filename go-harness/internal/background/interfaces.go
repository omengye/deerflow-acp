// Package background assembles Eino task lifecycle, durable ownership, fenced
// child stores and bounded workers. It never installs model-visible tools.
package background

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// Binding is immutable deployment-owned intent. SubmittedBy is audit data;
// access after reconnect is checked against the current session attachment.
type Binding struct {
	TaskID, ParentSessionID, ChildSessionID                  string
	SubmittedBy, OriginRunID, OriginToolCallID               string
	Workspace, ExecutionContract, RootBudgetID, AgentVersion string
	ConfigVersion                                            int64
	ExecutorKey, IntentHash, RequestHash                     string
	// Omitted for legacy root submissions so their intent hashes stay stable.
	// Ancestry is derived from a live managed attempt or host role placement.
	RootSessionID      string `json:",omitempty"`
	ParentTaskID       string `json:",omitempty"`
	Depth              int    `json:",omitempty"`
	ParentInvocationID string `json:",omitempty"`
	// Continuations use a new native task and session while retaining the
	// durable logical role. Only host-authorized continuation admission may
	// supply these fields; ordinary model submissions must leave them empty.
	ContinuationOf string `json:",omitempty"`
	InvocationID   string `json:",omitempty"`
}

func (b Binding) RoleID() string {
	if b.InvocationID != "" {
		return b.InvocationID
	}
	return "background/" + b.TaskID
}

func (b Binding) RootSession() string {
	if b.RootSessionID != "" {
		return b.RootSessionID
	}
	return b.ParentSessionID
}

func (b Binding) TaskDepth() int {
	if b.Depth > 0 {
		return b.Depth
	}
	return 1
}

type TaskScope struct {
	Binding Binding
	Attempt int64
}

// Authorizer must validate current owner/session attachment, including stale
// connection generations. A nil authorizer rejects all caller operations.
type Authorizer interface {
	AuthorizeTaskAccess(context.Context, harness.TaskActor) error
}

type Attempt struct {
	Agent adk.ResumableAgent
	// JoinAndClose must not return nil until all providers, tools and process
	// cleanup have joined. It must be safe even after Run/Resume fails.
	JoinAndClose func(context.Context) error
	// ExecutionFailure is inspected after JoinAndClose returns. It preserves
	// late provider/event failures whose I/O has definitely joined, without
	// treating an ordinary failed task as unconfirmed resource cleanup.
	ExecutionFailure func() error
	// ObserveControl runs before a native control request reaches the executor.
	// It lets the rebuilt agent distinguish drain checkpoints from cancellation.
	ObserveControl func(bt.ControlRequest)
	// HostControls revokes only this attempt's resource lease. It must never
	// carry model-supplied requests or replace durable cancellation intent.
	HostControls <-chan bt.ControlRequest
}

// AttemptFactory is deployment-owned and process-wide. Implementations rebuild
// resources from binding, never reuse one foreground invocation's closures.
type AttemptFactory interface {
	Validate(context.Context, Binding) error // repeatable, no external effects
	Open(context.Context, TaskScope) (*Attempt, error)
}

// ResourceReadiness pauses dispatch without claiming an attempt or consuming
// budget. The host repeats the check in the pending -> running transaction.
type ResourceReadiness interface {
	CheckResources(context.Context, Binding) error
}

// ResourceAdmission serializes a native claim and attempt construction against
// client resource retirement. Release must be idempotent: construction releases
// it once controls are installed, while the worker also releases on all errors.
type ResourceAdmission interface {
	BeginResourceAttempt(context.Context, string) (context.Context, func(), func(), error)
}

// BudgetLedger is the transaction seam for a separately implemented durable
// ledger shared by foreground and background work. Per-call reservations and
// settlement belong in the injected AttemptFactory's model/tool middleware.
// Tx methods MUST use tx and MUST NOT call the same DB's connection pool.
type BudgetLedger interface {
	BindTaskTx(context.Context, *sql.Tx, Binding) error
	BeforeAttempt(context.Context, TaskScope) error
	CommitAttemptTx(context.Context, *sql.Tx, TaskScope, string) error
}

// HeartbeatBudgetLedger extends the native task lease transaction to the root
// budget's active clock. Quota exhaustion must not abort this transaction:
// cleanup still owns a live attempt until CommitAttemptTx succeeds.
type HeartbeatBudgetLedger interface {
	HeartbeatAttemptTx(context.Context, *sql.Tx, TaskScope) error
}

// BudgetMonitor stops an attempt's provider/factory context when admission has
// closed. Native task heartbeats separately retain ownership through cleanup.
type BudgetMonitor interface {
	CheckAttemptBudget(context.Context, TaskScope) error
}

type ResumeGrant struct {
	Data     json.RawMessage
	Approval harness.TaskApproval
}

// PrepareResume reads/validates identity and intent. CommitResumeTx atomically
// consumes the approved request with the native waiting_input->pending change.
// The broker supplies native target data; clients never supply it directly.
type ApprovalBroker interface {
	PrepareResume(context.Context, harness.TaskActor, Binding, harness.TaskApproval) (ResumeGrant, error)
	CommitResumeTx(context.Context, *sql.Tx, harness.TaskActor, Binding, ResumeGrant) error
}

// Wakeup contains host-generated native targets for a saved descendant wait.
// Public clients cannot supply it. Commit repeats readiness and checkpoint
// validation in the native waiting_input -> pending transaction.
type Wakeup struct {
	TaskID  string
	Version int64
	Data    json.RawMessage
}

type WaitResumer interface {
	// Valid wakeups can accompany per-task errors. Callers process them before
	// reporting diagnostics so one damaged task cannot starve other sessions.
	ReadyWakeups(context.Context) ([]Wakeup, error)
	CommitWakeupTx(context.Context, *sql.Tx, Binding, Wakeup) error
}

type Config struct {
	Store      *sqlite.Store
	Authorizer Authorizer
	Budgets    BudgetLedger
	Approvals  ApprovalBroker
	Waits      WaitResumer
	// RolePlacement returns a live host-owned role for mixed delegation. A
	// nonnil transaction must be used without reentering the connection pool.
	RolePlacement func(context.Context, *sql.Tx) (*RolePlacement, error)
	Attempts      AttemptFactory
	// AgentNames are stable versioned registrations for the native durable
	// subagent executor. Nonempty names require Attempts.
	AgentNames []string
	// AdditionalExecutors supports explicit deployment executors and fixtures.
	// They share all binding, lease, budget and lifecycle transaction gates.
	AdditionalExecutors                                                    []bt.Executor
	MaxWorkers                                                             int
	PollInterval, HeartbeatInterval, NotificationLease, DrainCancelTimeout time.Duration
	MaxCheckpointBytes                                                     int
	OnError                                                                func(error)
	// OnTransitionTx extends non-heartbeat lifecycle transactions. On an active
	// attempt's final transition it runs after joined cleanup, budget settlement
	// and checkpoint promotion, but before the child lease is released. It also
	// observes pending/waiting cancellation and approval transitions. It must use
	// only tx, not the DB pool or CheckEffectTx (execution has already joined).
	// Returning an error rolls back native state, outbox, budget and projections.
	OnTransitionTx func(context.Context, *sql.Tx, TaskScope, *bt.Task, *bt.Task) error
}

type RolePlacement struct {
	ID, RootBudgetID string
	Depth            int
}

type Submission struct {
	Binding Binding
	Spec    bt.Spec
}

type attemptContextKey struct{}
type attemptContext struct {
	state   *attemptState
	attempt *Attempt
}

// ScopeFromContext gives deployment middleware the immutable authorization and
// budget identity of a managed attempt. Absence must fail closed for effects.
func ScopeFromContext(ctx context.Context) (TaskScope, bool) {
	v, ok := ctx.Value(attemptContextKey{}).(*attemptContext)
	if !ok || v == nil || v.state == nil {
		return TaskScope{}, false
	}
	return v.state.scope, true
}
