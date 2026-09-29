package background

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// LedgerAdapter binds native task lifetime to the durable root ledger. Both
// stores must use the same SQLite database; no budget transaction is nested.
type LedgerAdapter struct {
	store  *sqlite.Store
	ledger *budget.Ledger
}

func NewLedgerAdapter(store *sqlite.Store, ledger *budget.Ledger) (*LedgerAdapter, error) {
	if store == nil || ledger == nil {
		return nil, errors.New("background: durable store and budget ledger are required")
	}
	return &LedgerAdapter{store: store, ledger: ledger}, nil
}

// BudgetScope is host-generated and stable across reconstruction of the same
// attempt. A retry increments Fence and gets a new AttemptID; it retains root.
func BudgetScope(scope TaskScope) budget.Scope {
	return budget.Scope{RootBudgetID: scope.Binding.RootBudgetID, MemberID: scope.Binding.TaskID, SessionID: scope.Binding.ChildSessionID, AttemptID: scope.Binding.TaskID + "/attempt/" + strconv.FormatInt(scope.Attempt, 10), Fence: scope.Attempt}
}

// TaskScopeForBudgetTx reconstructs authority from the immutable durable
// binding, never from provider/model arguments. It performs no database-pool I/O.
func TaskScopeForBudgetTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) (TaskScope, error) {
	b, _, err := loadBinding(ctx, tx, scope.MemberID)
	if err != nil {
		return TaskScope{}, err
	}
	taskScope := TaskScope{Binding: b, Attempt: scope.Fence}
	if scope.Fence < 1 || BudgetScope(taskScope) != scope {
		return TaskScope{}, budget.ErrScope
	}
	return taskScope, nil
}

// CheckBudgetEffectTx is the background branch of budget.Config.CheckEffectTx.
// The host routes foreground scopes to its foreground fence independently.
func (s *Service) CheckBudgetEffectTx(ctx context.Context, tx *sql.Tx, scope budget.Scope) error {
	taskScope, err := TaskScopeForBudgetTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	return s.CheckEffectTx(ctx, tx, taskScope)
}

func (a *LedgerAdapter) BindTaskTx(ctx context.Context, tx *sql.Tx, b Binding) error {
	// Origin member's session must be the authenticated parent. Comparing only
	// roots would allow a sibling child's run to be substituted as authority.
	var rootID, sessionID string
	err := tx.QueryRowContext(ctx, "SELECT root_id,session_id FROM budget_members WHERE id=?", b.OriginRunID).Scan(&rootID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return budget.ErrScope
	}
	if err != nil {
		return ledgerPersistence("bind task origin", err)
	}
	if rootID != b.RootBudgetID || sessionID != b.ParentSessionID || b.ChildSessionID == "" || b.ChildSessionID == b.ParentSessionID {
		return budget.ErrScope
	}
	return a.ledger.BindMemberTx(ctx, tx, budget.MemberBinding{RootBudgetID: rootID, MemberID: b.TaskID, SessionID: b.ChildSessionID, OriginRunID: b.OriginRunID, ParentMemberID: b.OriginRunID, Kind: "task"})
}

func (a *LedgerAdapter) checkTaskTx(ctx context.Context, tx *sql.Tx, scope TaskScope, allowStopping bool) error {
	actual, blocked, err := loadBinding(ctx, tx, scope.Binding.TaskID)
	if err != nil {
		return err
	}
	if actual != scope.Binding || scope.Attempt < 1 {
		return budget.ErrScope
	}
	if blocked != "" && !allowStopping {
		return budget.ErrUnknown
	}
	if err = a.store.Tasks().CheckAttemptTx(ctx, tx, actual.TaskID, scope.Attempt, allowStopping); err != nil {
		return err
	}
	var taskID string
	var attempt int64
	if err = tx.QueryRowContext(ctx, "SELECT task_id,attempt FROM harness_background_child_leases WHERE child_session_id=?", actual.ChildSessionID).Scan(&taskID, &attempt); errors.Is(err, sql.ErrNoRows) {
		return bt.ErrLeaseLost
	} else if err != nil {
		return ledgerPersistence("check child lease", err)
	}
	if taskID != actual.TaskID || attempt != scope.Attempt {
		return bt.ErrLeaseLost
	}
	return nil
}

func (a *LedgerAdapter) BeforeAttempt(ctx context.Context, scope TaskScope) error {
	tx, err := a.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return ledgerPersistence("begin task attempt", err)
	}
	defer tx.Rollback()
	if err = a.BeforeAttemptTx(ctx, tx, scope); err != nil {
		return err
	}
	return ledgerPersistence("commit task attempt", tx.Commit())
}

// BeforeAttemptTx permits the host to make its child business-run projection
// runnable in the same transaction as budget admission and task/child fencing.
// It exclusively uses tx; the caller owns commit and rollback.
func (a *LedgerAdapter) BeforeAttemptTx(ctx context.Context, tx *sql.Tx, scope TaskScope) error {
	if err := a.checkTaskTx(ctx, tx, scope, false); err != nil {
		return err
	}
	return a.ledger.BeginAttemptTx(ctx, tx, BudgetScope(scope))
}

func (a *LedgerAdapter) attemptExistsTx(ctx context.Context, tx *sql.Tx, scope TaskScope) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM budget_attempts WHERE id=?", BudgetScope(scope).AttemptID).Scan(&count)
	return count == 1, ledgerPersistence("read task attempt", err)
}

func (a *LedgerAdapter) HeartbeatAttemptTx(ctx context.Context, tx *sql.Tx, scope TaskScope) error {
	if err := a.checkTaskTx(ctx, tx, scope, true); err != nil {
		return err
	}
	exists, err := a.attemptExistsTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	// The native heartbeat may race the first BeforeAttempt transaction. There
	// is no budget clock or external effect to renew before admission completes.
	if !exists {
		return nil
	}
	return a.ledger.HeartbeatTx(ctx, tx, BudgetScope(scope))
}

// HeartbeatInterval is an upper bound for the host's native heartbeat interval.
func (a *LedgerAdapter) HeartbeatInterval() time.Duration { return a.ledger.HeartbeatInterval() }

func (a *LedgerAdapter) CheckAttemptBudget(ctx context.Context, scope TaskScope) error {
	err := a.ledger.Validate(ctx, BudgetScope(scope))
	if !errors.Is(err, bt.ErrLeaseLost) {
		return err
	}
	// Durable cancellation closes new-effect admission while cleanup still
	// owns the native and child leases. Validate wraps this normal fence denial
	// as a persistence error; confirm the exact live cancellation before
	// classifying it as an ordinary stop. Real lease loss stays diagnostic.
	tx, readErr := a.store.DB().BeginTx(ctx, nil)
	if readErr != nil {
		return errors.Join(err, ledgerPersistence("inspect canceled budget attempt", readErr))
	}
	defer tx.Rollback()
	if readErr = a.checkTaskTx(ctx, tx, scope, true); readErr != nil {
		return errors.Join(err, readErr)
	}
	task, _, readErr := a.store.Tasks().TaskLeaseTx(ctx, tx, scope.Binding.TaskID)
	if readErr != nil {
		return errors.Join(err, ledgerPersistence("inspect canceled budget attempt", readErr))
	}
	if task.CancelRequestedAt != nil {
		return context.Canceled
	}
	return err
}

func (a *LedgerAdapter) CommitAttemptTx(ctx context.Context, tx *sql.Tx, scope TaskScope, status string) error {
	if err := a.checkTaskTx(ctx, tx, scope, true); err != nil {
		return err
	}
	var outcome budget.Outcome
	switch bt.Status(status) {
	case bt.StatusCompleted:
		outcome = budget.OutcomeCompleted
	case bt.StatusFailed:
		outcome = budget.OutcomeFailed
	case bt.StatusCanceled:
		outcome = budget.OutcomeCancelled
	case bt.StatusWaitingInput, bt.StatusSuspended, bt.StatusPending:
		outcome = budget.OutcomeWaitingInput
	default:
		return budget.ErrScope
	}
	exists, err := a.attemptExistsTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !exists {
		// Admission failed before opening any attempt-owned resources. Publishing
		// that failure is safe; a success/checkpoint still requires ledger proof.
		if outcome == budget.OutcomeFailed || outcome == budget.OutcomeCancelled {
			return nil
		}
		return budget.ErrScope
	}
	return a.ledger.EndAttemptTx(ctx, tx, BudgetScope(scope), outcome)
}

func ledgerPersistence(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &budget.PersistenceError{Operation: operation, Err: err}
}

var _ BudgetLedger = (*LedgerAdapter)(nil)
var _ HeartbeatBudgetLedger = (*LedgerAdapter)(nil)
var _ BudgetMonitor = (*LedgerAdapter)(nil)
