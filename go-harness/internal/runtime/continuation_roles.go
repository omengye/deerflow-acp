package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	bt "github.com/cloudwego/eino/adk/backgroundtask"

	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

var errRoleConversationUnavailable = errors.New("parent role conversation is not committed")

func unavailableRoleConversation(message string) error {
	return errors.Join(errRoleConversationUnavailable, continuationConflict(message))
}

func continuationParentRoleTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec) (savedRoleContext, error) {
	var empty savedRoleContext
	if spec.Version < 5 || b.ParentInvocationID == "" || b.ParentTaskID != "" || b.ParentSessionID != b.RootSession() {
		return empty, unavailableRoleConversation("parent role requires its original execution host")
	}
	saved, err := readRoleContextTx(ctx, tx, b.RootBudgetID, b.ParentInvocationID, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return saved, unavailableRoleConversation("parent role has no committed conversation")
	}
	if err != nil {
		return saved, err
	}
	if saved.Context.SessionID != b.ParentSessionID || saved.Context.Invocation.Depth+1 != b.TaskDepth() {
		return saved, continuationConflict("parent conversation scope changed")
	}
	return saved, nil
}

func validateContinuationRoleTx(ctx context.Context, tx *sql.Tx, source continuationSource) error {
	target, derived, err := notificationRecipientTx(ctx, tx, source.Binding, source.Spec, source.Notification)
	if err != nil {
		return err
	}
	if derived != source.DerivedFrom || (derived != "") != (source.Version == 3) {
		return continuationConflict("derived role result source changed")
	}
	if source.ParentRole == nil {
		if target != "" || source.Binding.ParentTaskID != "" || source.Version != 1 && source.Version != 3 {
			return continuationConflict("parent role source is missing")
		}
		return nil
	}
	role := source.ParentRole
	b := source.Binding
	if (source.Version != 2 && source.Version != 3) || source.Spec.Version < 5 || b.ParentTaskID != "" || role.Context.SessionID != source.SessionID || target != role.Context.Invocation.ID || role.Revision < 1 {
		return continuationConflict("parent role source changed")
	}
	saved, err := readRoleContextTx(ctx, tx, b.RootBudgetID, target, role.Revision)
	if err != nil {
		return err
	}
	actual, _ := json.Marshal(role)
	want, _ := json.Marshal(saved)
	if !bytes.Equal(actual, want) {
		return continuationConflict("parent conversation changed")
	}
	return nil
}

func notificationRecipientTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, n bt.Notification) (string, string, error) {
	if n.Kind == roleResultNotification {
		result, err := readRoleResultTx(ctx, tx, b, spec, n)
		return result.RecipientRole, result.SourceRunID, err
	}
	return b.ParentInvocationID, "", nil
}

func notificationParentRoleTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec, n bt.Notification) (*savedRoleContext, string, error) {
	target, derived, err := notificationRecipientTx(ctx, tx, b, spec, n)
	if err != nil || target == "" {
		return nil, derived, err
	}
	var saved savedRoleContext
	if derived == "" {
		saved, err = continuationParentRoleTx(ctx, tx, b, spec)
	} else {
		saved, err = readRoleContextTx(ctx, tx, b.RootBudgetID, target, 0)
		if err == nil && saved.Context.SessionID != b.ParentSessionID {
			err = continuationConflict("ancestor conversation scope changed")
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = unavailableRoleConversation("parent role has no committed conversation")
	}
	return &saved, derived, err
}
