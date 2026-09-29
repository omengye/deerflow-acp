// Package budget owns the durable, host-authorized budget shared by a logical
// foreground execution and all of its delegated work. Checkpoints are not quota
// authority. All Tx methods exclusively use the caller's SQL transaction.
package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var (
	ErrScope    = errors.New("invalid or stale budget scope")
	ErrConflict = errors.New("budget identity or idempotency conflict")
	ErrUnknown  = errors.New("budget has unresolved interrupted work")
	ErrClock    = errors.New("budget clock moved backwards")
)

type Config struct {
	// CheckEffectTx checks external task/lease fencing in the SAME transaction
	// used for admission and dispatch. It must never open another DB transaction.
	CheckEffectTx func(context.Context, *sql.Tx, Scope) error
	// Optional lease check that permits cancellation cleanup. Heartbeats do not
	// call CheckEffectTx, because cleanup may no longer admit new effects.
	CheckHeartbeatTx func(context.Context, *sql.Tx, Scope) error
	Now              func() time.Time
	LeaseDuration    time.Duration
}

type RootSpec struct {
	RootBudgetID, SessionID, RootRunID, PolicyHash string
	Limits                                         harness.BudgetLimits
}

type MemberBinding struct {
	RootBudgetID, MemberID, SessionID string
	// OriginRunID must identify an existing member of this root. For delegated
	// work ParentMemberID must also be that member; identities are host supplied.
	OriginRunID, ParentMemberID, Kind string
}

type Scope struct {
	RootBudgetID, MemberID, SessionID, AttemptID string
	Fence                                        int64
}

type Outcome string

const (
	OutcomeCompleted    Outcome = "completed"
	OutcomeFailed       Outcome = "failed"
	OutcomeCancelled    Outcome = "cancelled"
	OutcomeWaitingInput Outcome = "waiting_input"
	OutcomeInterrupted  Outcome = "interrupted"
)

type Snapshot struct {
	RootBudgetID, SessionID, RootRunID, PolicyHash string
	Limits                                         harness.BudgetLimits
	ModelCalls, ToolCalls                          int
	SpentTokens, HeldTokens                        int64
	Elapsed                                        time.Duration
	Revision                                       int64
	BlockedReason                                  string
}

// Identity is safe to put in a checkpoint. A newer ledger revision is normal;
// a ledger older than its checkpoint or a policy mismatch fails closed.
type Identity struct {
	RootBudgetID, PolicyHash string
	Revision                 int64
}

type ModelRequest struct {
	OperationID, Digest string
	InputTokens         int64
	MaxOutputTokens     int
}

type ToolRequest struct{ OperationID, Digest string }

type Reservation struct {
	OperationID             string
	Scope                   Scope
	Kind, State             string
	InputTokens, HeldTokens int64
	MaxOutputTokens         int
}

type Settlement struct {
	Usage harness.Usage
	// Complete means actual I/O has joined cleanly. Missing final usage on a
	// failed/cancelled call conservatively charges its entire held amount.
	Complete bool
	// Unknown keeps the hold, blocks new effects, and requires reconciliation.
	Unknown bool
}

type LimitError struct {
	Resource  string
	Temporary bool
}

func (e *LimitError) Error() string {
	if e.Temporary {
		return "shared budget temporarily held by in-flight work: " + e.Resource
	}
	return "shared " + e.Resource + " budget exhausted"
}

// PersistenceError is atomic during cancellation-error filtering. In
// particular a SQL deadline/cancellation must never become a successful stop.
type PersistenceError struct {
	Operation string
	Err       error
}

func (e *PersistenceError) Error() string {
	return fmt.Sprintf("budget %s persistence: %v", e.Operation, e.Err)
}
func (e *PersistenceError) Unwrap() error            { return e.Err }
func (e *PersistenceError) PersistenceFailure() bool { return true }

type scopeKey struct{}

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}
func ScopeFromContext(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	return s, ok
}

// The constructor creates the independent budget tables. Lifecycle methods are
// CreateRootTx, BindMemberTx, BeginAttemptTx, EndAttemptTx and
// ReconcileInterruptedTx(ctx, tx). Non-Tx counterparts are available for hosts
// that do not need to commit a business row atomically with the lifecycle.
