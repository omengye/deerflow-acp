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
}

// AttemptFactory is deployment-owned and process-wide. Implementations rebuild
// resources from binding, never reuse one foreground invocation's closures.
type AttemptFactory interface {
	Validate(context.Context, Binding) error // repeatable, no external effects
	Open(context.Context, TaskScope) (*Attempt, error)
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

type Config struct {
	Store      *sqlite.Store
	Authorizer Authorizer
	Budgets    BudgetLedger
	Approvals  ApprovalBroker
	Attempts   AttemptFactory
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
