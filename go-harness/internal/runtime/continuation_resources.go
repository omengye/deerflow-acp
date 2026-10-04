package runtime

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

// ContinuationResourceSource validates source ownership and effect certainty
// before the host performs any resource discovery. It creates no execution,
// budget attempt, permission grant or automatic-continuation charge.
func (s *Service) ContinuationResourceSource(ctx context.Context, owner, rootSession, notificationID, runID string) (binding background.Binding, spec BackgroundExecutionSpec, found bool, err error) {
	if err = s.Coordinator.Authorize(rootSession, owner); err != nil {
		return
	}
	if (notificationID == "") == (runID == "") {
		err = harness.ErrInvalidInput
		return
	}
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var recipient string
		if runID != "" {
			row, e := readExecution(ctx, tx, rootSession, runID)
			if e != nil {
				return e
			}
			source, e := loadContinuationSource(ctx, tx, row)
			if e != nil || source == nil {
				return e
			}
			binding, spec = source.Binding, source.Spec
			if source.ParentRole != nil {
				recipient = source.ParentRole.Context.Invocation.ID
			}
		} else {
			var parent string
			if e := tx.QueryRowContext(ctx, `SELECT parent_session_id FROM harness_background_inbox WHERE notification_id=?`, notificationID).Scan(&parent); e != nil {
				return e
			}
			note, e := readContinuationNotification(ctx, tx, parent, notificationID)
			if e != nil {
				return e
			}
			binding, spec, _, _, e = readContinuationOrigin(ctx, tx, parent, note.TaskID)
			if e != nil {
				return e
			}
			if binding.RootSession() != rootSession {
				return harness.ErrPermissionDenied
			}
			if binding.ParentTaskID != "" || binding.ParentSessionID != binding.RootSession() {
				target, e := backgroundContinuationTargetTx(ctx, tx, binding, spec)
				if e != nil {
					return e
				}
				recipient = target.Role.Context.Invocation.ID
			} else {
				role, _, e := notificationParentRoleTx(ctx, tx, binding, spec, note)
				if e != nil {
					return e
				}
				if role != nil {
					recipient = role.Context.Invocation.ID
				}
			}
		}
		if binding.RootSession() != rootSession {
			return harness.ErrPermissionDenied
		}
		var uncertain bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('started','uncertain'))`, rootSession).Scan(&uncertain); e != nil {
			return e
		}
		if uncertain {
			return harness.ErrReconciliationRequired
		}
		if e := agents.CheckStoppedIfConfigured(ctx, tx, binding.RootBudgetID, recipient); e != nil {
			return e
		}
		found = true
		return nil
	})
	return
}
