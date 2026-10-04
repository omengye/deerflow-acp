package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

var errBackgroundParentStopped = fmt.Errorf("%w: background parent was stopped", harness.ErrPermissionDenied)

func blockedContinuationSource(err error) bool {
	// An explicit stop is a policy denial for approval/read projections, but
	// the scheduler records it separately from damaged continuation sources.
	if errors.Is(err, errBackgroundParentStopped) {
		return false
	}
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	return errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrExecutionConflict) || errors.Is(err, harness.ErrReconciliationRequired) || errors.Is(err, harness.ErrTaskOriginConflict) || errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || errors.As(err, &syntax) || errors.As(err, &shape)
}

// A target is read from the original recipient's durable host. It is an
// execution recipe, never authority to reuse the completed native task lease.
// New-run admission must repeat this check in its own creation transaction.
type backgroundContinuationTarget struct {
	Role        savedRoleContext
	Host        background.Binding
	HostSpec    BackgroundExecutionSpec
	HostVersion int64
}

func backgroundContinuationTargetTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec) (backgroundContinuationTarget, error) {
	var target backgroundContinuationTarget
	if spec.Version < 5 || b.ParentInvocationID == "" || b.ParentTaskID == "" || b.ParentSessionID == b.RootSession() {
		return target, unavailableRoleConversation("background parent has no supported role host")
	}
	if _, err := validateBackgroundSpec(b, spec); err != nil {
		return target, continuationConflict("background child policy changed")
	}
	// Resolve the nearest native host independently of the role snapshot. A
	// foreground role inside a background task shares its host's session but
	// retains its own role ID and conversation.
	hostStore := &BackgroundExecutionStore{}
	parent, err := hostStore.ParentRoleTx(ctx, tx, b, spec)
	if err != nil {
		return target, err
	}
	host, hostSpec, err := hostStore.parentSpecTx(ctx, tx, b, spec)
	if err != nil {
		return target, err
	}
	if hostSpec.Version < 5 || hostSpec.Role == nil || host.ChildSessionID != b.ParentSessionID || host.RootSession() != b.RootSession() || host.RootBudgetID != b.RootBudgetID || hostSpec.Roles == nil || spec.Roles == nil || hostSpec.Roles.Digest != spec.Roles.Digest {
		return target, continuationConflict("background parent host changed")
	}
	ownerRole, err := agents.ReadInvocation(ctx, tx, host.RoleID(), b.RootBudgetID)
	if err != nil {
		return target, err
	}
	if ownerRole.Depth != host.TaskDepth() || ownerRole.Spec.Version != hostSpec.Role.Version {
		return target, continuationConflict("background host role changed")
	}
	owned, err := agents.IsDescendant(ctx, tx, b.RootBudgetID, ownerRole.ID, b.ParentInvocationID)
	if err != nil {
		return target, err
	}
	if !owned {
		return target, continuationConflict("recipient belongs to another background host")
	}
	if err = hostStore.CheckParentPolicyTx(ctx, tx, host, hostSpec); err != nil {
		return target, err
	}
	if _, err = backgroundChildPolicy(ctx, tx, host, hostSpec); err != nil {
		return target, err
	}
	role, err := readRoleContextTx(ctx, tx, b.RootBudgetID, b.ParentInvocationID, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return target, unavailableRoleConversation("background parent has no committed conversation")
	}
	if err != nil {
		return target, err
	}
	if role.Context.RunID != host.TaskID {
		// A prior notification may have advanced this same logical role in
		// another native host. Only a validated continuation can replace it.
		var data, encoded []byte
		if err = tx.QueryRowContext(ctx, `SELECT binding,payload FROM harness_background_specs WHERE task_id=?`, role.Context.RunID).Scan(&data, &encoded); err != nil {
			return target, err
		}
		var resumed background.Binding
		var resumedSpec BackgroundExecutionSpec
		if json.Unmarshal(data, &resumed) != nil || json.Unmarshal(encoded, &resumedSpec) != nil || resumed.ContinuationOf == "" || resumed.RoleID() != role.Context.Invocation.ID || resumed.RootSession() != host.RootSession() || resumed.RootBudgetID != host.RootBudgetID || resumedSpec.Roles == nil || resumedSpec.Roles.Digest != hostSpec.Roles.Digest {
			return target, continuationConflict("background parent context belongs to another host")
		}
		if _, err = validateBackgroundSpec(resumed, resumedSpec); err != nil {
			return target, err
		}
		if _, err = readBackgroundContinuationSourceTx(ctx, tx, resumed, resumedSpec); err != nil {
			return target, err
		}
		host, hostSpec = resumed, resumedSpec
	}
	if role.Context.SessionID != host.ChildSessionID || role.Context.RunID != host.TaskID || role.Context.Invocation.Depth+1 != b.TaskDepth() || role.Context.Invocation.Spec.Version != parent.Version {
		return target, continuationConflict("background parent conversation changed")
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_continuation_sources c JOIN eino_background_tasks t ON t.id=c.task_id WHERE c.root_id=? AND c.role_id=? AND t.status NOT IN ('completed','failed','canceled'))`, b.RootBudgetID, role.Context.Invocation.ID).Scan(&active); err != nil {
		return target, err
	}
	if active {
		return target, unavailableRoleConversation("background parent continuation is active")
	}
	native, err := readBackgroundNativeTask(ctx, tx, host.TaskID)
	if err != nil {
		return target, err
	}
	var stopped, rootStopped bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_stops WHERE task_id=?), EXISTS(SELECT 1 FROM harness_automatic_stops WHERE root_id=?)`, host.TaskID, b.RootBudgetID).Scan(&stopped, &rootStopped); err != nil {
		return target, err
	}
	automatic, _ := ctx.Value(automaticNotificationKey{}).(bool)
	if stopped || automatic && rootStopped || native.Status == bt.StatusCanceled {
		return target, errBackgroundParentStopped
	}
	if !terminalBackgroundStatus(string(native.Status)) {
		return target, unavailableRoleConversation("background parent execution has not finished")
	}
	var runStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM harness_runs WHERE id=? AND session_id=?`, host.TaskID, host.ChildSessionID).Scan(&runStatus); err != nil {
		return target, err
	}
	if native.DoneAt == nil || !terminalBackgroundRun(string(native.Status), runStatus) {
		return target, continuationConflict("background parent terminal state changed")
	}
	var uncertain, occupied bool
	if err = tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('started','uncertain')) OR
 EXISTS(SELECT 1 FROM harness_background_execution_failures WHERE task_id=? AND persistence_failure<>0),
 EXISTS(SELECT 1 FROM harness_background_child_leases WHERE child_session_id=?) OR
 EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state IN ('active','unknown')) OR
 EXISTS(SELECT 1 FROM budget_reservations WHERE member_id=? AND state IN ('reserved','dispatched','unknown'))`, host.ChildSessionID, host.TaskID, host.ChildSessionID, host.TaskID, host.TaskID).Scan(&uncertain, &occupied); err != nil {
		return target, err
	}
	if uncertain {
		return target, harness.ErrReconciliationRequired
	}
	if occupied {
		return target, unavailableRoleConversation("background parent resources have not settled")
	}
	return backgroundContinuationTarget{Role: role, Host: host, HostSpec: hostSpec, HostVersion: native.Version}, nil
}

// Repeat the snapshot/host/version check immediately before admitting a new
// execution. No model callback or connection-owned permission is retained.
func validateBackgroundContinuationTargetTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, pinned backgroundContinuationTarget) error {
	current, err := backgroundContinuationTargetTx(ctx, tx, b, spec)
	if err != nil {
		return err
	}
	want, err := json.Marshal(pinned)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if string(want) != string(actual) {
		return continuationConflict("background parent target changed before admission")
	}
	return nil
}
