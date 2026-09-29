package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

// prepareBackgroundGraphTx checks every task and child before any graph row is
// removed. Background children are private; a task may reuse a child session,
// so each child is purged once after all of its task rows are removed.
func prepareBackgroundGraphTx(ctx context.Context, tx *sql.Tx, parent string, tables map[string]bool, automatic bool) ([]string, []string, error) {
	if tables["harness_background_specs"] {
		var child bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs WHERE child_session_id=?)`, parent).Scan(&child); err != nil {
			return nil, nil, err
		}
		if child {
			return nil, nil, harness.ErrInvalidInput
		}
	}
	if !tables["harness_background_bindings"] {
		if tables["harness_background_inbox"] {
			var inbox bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_inbox WHERE parent_session_id=?)`, parent).Scan(&inbox); err != nil || inbox {
				return nil, nil, graphBlocked(err)
			}
		}
		if tables["harness_background_specs"] && tables["budget_members"] && tables["budget_roots"] {
			var specs bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs s JOIN budget_members m ON m.id=s.task_id JOIN budget_roots root ON root.id=m.root_id WHERE root.session_id=?)`, parent).Scan(&specs); err != nil || specs {
				return nil, nil, graphBlocked(err)
			}
		}
		return nil, nil, nil
	}
	var isChild bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_bindings WHERE child_session_id=?)`, parent).Scan(&isChild); err != nil {
		return nil, nil, err
	}
	if isChild {
		return nil, nil, harness.ErrInvalidInput
	}
	rows, err := tx.QueryContext(ctx, `SELECT task_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload,blocked_reason FROM harness_background_bindings WHERE parent_session_id=? ORDER BY task_id`, parent)
	if err != nil {
		return nil, nil, err
	}
	type taskBinding struct {
		id, child, origin, call, intent, blocked string
		payload                                  []byte
	}
	var bindings []taskBinding
	for rows.Next() {
		var b taskBinding
		if err = rows.Scan(&b.id, &b.child, &b.origin, &b.call, &b.intent, &b.payload, &b.blocked); err != nil {
			break
		}
		bindings = append(bindings, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if tables["harness_background_inbox"] {
		// A stale inbox row with no bound task cannot be removed safely by task ID.
		var orphan bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_inbox i WHERE i.parent_session_id=? AND NOT EXISTS(SELECT 1 FROM harness_background_bindings b WHERE b.task_id=i.task_id AND b.parent_session_id=?))`, parent, parent).Scan(&orphan); err != nil || orphan {
			return nil, nil, graphBlocked(err)
		}
	}
	if tables["harness_background_specs"] && tables["budget_members"] && tables["budget_roots"] {
		var orphan bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs s JOIN budget_members m ON m.id=s.task_id JOIN budget_roots root ON root.id=m.root_id WHERE root.session_id=? AND NOT EXISTS(SELECT 1 FROM harness_background_bindings b WHERE b.task_id=s.task_id AND b.parent_session_id=?))`, parent, parent).Scan(&orphan)
		if err != nil || orphan {
			return nil, nil, graphBlocked(err)
		}
	}
	if len(bindings) == 0 {
		return nil, nil, nil
	}
	for _, name := range []string{"harness_background_specs", "eino_background_tasks", "budget_members", "budget_roots", "budget_attempts", "budget_reservations", "harness_runs", "harness_tool_receipts", "eino_task_notifications", "harness_background_child_leases", "harness_background_inbox"} {
		if !tables[name] {
			return nil, nil, fmt.Errorf("%w: background graph lacks %s", harness.ErrBusy, name)
		}
	}
	seenChildren := make(map[string]bool)
	var children, tasks []string
	for _, b := range bindings {
		if b.id == "" || b.child == "" || b.child == parent || b.blocked != "" {
			return nil, nil, harness.ErrBusy
		}
		var binding background.Binding
		if json.Unmarshal(b.payload, &binding) != nil || binding.TaskID != b.id || binding.ParentSessionID != parent || binding.ChildSessionID != b.child || binding.OriginRunID != b.origin || binding.OriginToolCallID != b.call || binding.IntentHash != b.intent {
			return nil, nil, harness.ErrBusy
		}
		var status string
		var version, lease int64
		var payload []byte
		err = tx.QueryRowContext(ctx, `SELECT status,version,lease_expires_at,payload FROM eino_background_tasks WHERE id=?`, b.id).Scan(&status, &version, &lease, &payload)
		if err != nil {
			return nil, nil, graphBlocked(err)
		}
		var snapshot bt.Task
		if json.Unmarshal(payload, &snapshot) != nil || snapshot.Spec.ID != b.id || snapshot.Spec.SessionID != parent || snapshot.Version != version || string(snapshot.Status) != status || snapshot.DoneAt == nil || lease != 0 || !terminalBackgroundStatus(status) {
			return nil, nil, harness.ErrBusy
		}
		var specChild, contract string
		var specData, specBinding []byte
		if err = tx.QueryRowContext(ctx, `SELECT child_session_id,contract,payload,binding FROM harness_background_specs WHERE task_id=?`, b.id).Scan(&specChild, &contract, &specData, &specBinding); err != nil || specChild != b.child || contract != binding.ExecutionContract {
			return nil, nil, graphBlocked(err)
		}
		var savedBinding background.Binding
		var spec BackgroundExecutionSpec
		if json.Unmarshal(specBinding, &savedBinding) != nil || savedBinding != binding || json.Unmarshal(specData, &spec) != nil || spec.Parent.ID != parent {
			return nil, nil, harness.ErrBusy
		}
		calculated, contractErr := spec.Contract()
		if contractErr != nil || calculated != contract {
			return nil, nil, harness.ErrBusy
		}
		var runSession, runStatus, budgetSession, budgetBlocked string
		err = tx.QueryRowContext(ctx, `SELECT r.session_id,r.status,root.session_id,root.blocked_reason FROM harness_runs r JOIN budget_members m ON m.id=r.id JOIN budget_roots root ON root.id=m.root_id WHERE r.id=?`, b.id).Scan(&runSession, &runStatus, &budgetSession, &budgetBlocked)
		if err != nil || runSession != b.child || budgetSession != parent || budgetBlocked != "" || !terminalBackgroundRun(status, runStatus) {
			return nil, nil, graphBlocked(err)
		}
		for _, check := range []struct{ query, id string }{
			{`SELECT EXISTS(SELECT 1 FROM harness_background_child_leases WHERE task_id=?)`, b.id},
			{`SELECT EXISTS(SELECT 1 FROM eino_task_notifications WHERE task_id=?)`, b.id},
			{`SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('pending','started','uncertain'))`, b.child},
			{`SELECT EXISTS(SELECT 1 FROM budget_attempts WHERE member_id=? AND state IN ('active','unknown'))`, b.id},
			{`SELECT EXISTS(SELECT 1 FROM budget_reservations WHERE member_id=? AND state IN ('reserved','dispatched','unknown'))`, b.id},
		} {
			var blocked bool
			if err = tx.QueryRowContext(ctx, check.query, check.id).Scan(&blocked); err != nil || blocked {
				return nil, nil, graphBlocked(err)
			}
		}
		if tables["harness_background_execution_failures"] {
			var uncertain bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_execution_failures WHERE task_id=? AND persistence_failure<>0)`, b.id).Scan(&uncertain); err != nil || uncertain {
				return nil, nil, graphBlocked(err)
			}
		}
		if automatic {
			var unhandled bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_inbox WHERE task_id=? AND handled=0)`, b.id).Scan(&unhandled); err != nil || unhandled {
				return nil, nil, graphBlocked(err)
			}
		}
		if !seenChildren[b.child] {
			seenChildren[b.child] = true
			children = append(children, b.child)
		}
		tasks = append(tasks, b.id)
	}
	for _, child := range children {
		var shared bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs s LEFT JOIN harness_background_bindings b ON b.task_id=s.task_id WHERE s.child_session_id=? AND (b.parent_session_id IS NULL OR b.parent_session_id<>?))`, child, parent).Scan(&shared)
		if err != nil || shared {
			return nil, nil, graphBlocked(err)
		}
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_bindings WHERE parent_session_id=?)`, child).Scan(&shared)
		if err != nil || shared {
			return nil, nil, graphBlocked(err)
		}
	}
	return children, tasks, nil
}

func graphBlocked(err error) error {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return harness.ErrBusy
	}
	return err
}

func terminalBackgroundStatus(status string) bool {
	switch bt.Status(status) {
	case bt.StatusCompleted, bt.StatusFailed, bt.StatusCanceled:
		return true
	default:
		return false
	}
}

func terminalBackgroundRun(taskStatus, runStatus string) bool {
	switch bt.Status(taskStatus) {
	case bt.StatusCompleted:
		return runStatus == "completed"
	case bt.StatusFailed:
		return runStatus == "failed"
	case bt.StatusCanceled:
		return runStatus == "cancelled"
	default:
		return false
	}
}

func purgeBackgroundGraphTx(ctx context.Context, tx *sql.Tx, tasks []string, tables map[string]bool) error {
	steps := []struct{ table, query string }{
		{"harness_background_policy_grants", `DELETE FROM harness_background_policy_grants WHERE grant_id IN (SELECT id FROM harness_background_permission_grants WHERE task_id=?)`},
		{"harness_background_permission_grants", `DELETE FROM harness_background_permission_grants WHERE task_id=?`},
		{"harness_background_permission_intents", `DELETE FROM harness_background_permission_intents WHERE task_id=?`},
		{"harness_background_permission_manifests", `DELETE FROM harness_background_permission_manifests WHERE task_id=?`},
		{"harness_background_interrupt_stages", `DELETE FROM harness_background_interrupt_stages WHERE task_id=?`},
		{"harness_background_drain_manifests", `DELETE FROM harness_background_drain_manifests WHERE task_id=?`},
		{"harness_background_engine_contracts", `DELETE FROM harness_background_engine_contracts WHERE task_id=?`},
		{"harness_background_execution_failures", `DELETE FROM harness_background_execution_failures WHERE task_id=?`},
		{"harness_background_inbox", `DELETE FROM harness_background_inbox WHERE task_id=?`},
		{"eino_task_notifications", `DELETE FROM eino_task_notifications WHERE task_id=?`},
		{"eino_notification_replays", `DELETE FROM eino_notification_replays WHERE task_id=?`},
		{"eino_task_events", `DELETE FROM eino_task_events WHERE task_id=?`},
		{"harness_background_child_leases", `DELETE FROM harness_background_child_leases WHERE task_id=?`},
		{"harness_background_specs", `DELETE FROM harness_background_specs WHERE task_id=?`},
		{"harness_background_bindings", `DELETE FROM harness_background_bindings WHERE task_id=?`},
		{"eino_checkpoints", `DELETE FROM eino_checkpoints WHERE id=?||'/checkpoint'`},
		{"eino_background_tasks", `DELETE FROM eino_background_tasks WHERE id=?`},
	}
	for _, id := range tasks {
		for _, step := range steps {
			if !tables[step.table] {
				continue
			}
			if _, err := tx.ExecContext(ctx, step.query, id); err != nil {
				return err
			}
		}
	}
	return nil
}
