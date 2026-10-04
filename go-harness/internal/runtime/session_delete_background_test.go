package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

func completedGraphFixture(t *testing.T) *backgroundHostFixture {
	t.Helper()
	f := newBackgroundHostFixture(t)
	ctx := context.Background()
	if err := f.create(); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if err := f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ParentSessionID, RunID: f.binding.OriginRunID, Kind: "tool_end", ToolCallID: f.binding.OriginToolCallID, ToolName: "background_agent", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	// The native delivery service has acknowledged every outbox item. A leased
	// or undelivered row must block graph deletion in the tests below.
	if _, err = f.native.DB().ExecContext(ctx, `DELETE FROM eino_task_notifications WHERE task_id=?`, f.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDeleteTerminalBackgroundGraphAtomically(t *testing.T) {
	f := newBackgroundHostFixture(t)
	ctx := context.Background()
	if err := f.create(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.purgeSession(ctx, f.binding.ParentSessionID, false); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("pending task deleted: %v", err)
	}
	// A failed delete must preserve every graph row.
	for table, idColumn := range map[string]string{"harness_sessions": "id", "eino_background_tasks": "id", "harness_background_specs": "task_id"} {
		id := f.binding.TaskID
		if table == "harness_sessions" {
			id = f.binding.ChildSessionID
		}
		var count int
		if err := f.native.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE `+idColumn+`=?`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s after blocked delete: count=%d err=%v", table, count, err)
		}
	}
	// This fixture's task is still pending; use a separate completed graph for
	// the successful transaction and its foreign-key ordering.
	done := completedGraphFixture(t)
	if _, err := NewBackgroundInteractionStore(ctx, done.store, &backgroundPermissionAuthority{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES('task-child','historical','task-child/session','completed',1,x'7b7d')`,
		`INSERT INTO harness_background_permission_intents(id,task_id,tool_call_id,version,state,request,descriptor) VALUES('historical-intent','task-child','historical',1,'decided',x'7b7d',x'7b7d')`,
		`INSERT INTO harness_background_permission_grants(id,task_id,attempt,intent_id,intent_version,decision,state,approved_by,task_version) VALUES('historical-grant','task-child',1,'historical-intent',1,'allow_once','used','fixture',3)`,
		`INSERT INTO harness_background_policy_grants(grant_id,policy_sha,config_version) VALUES('historical-grant','hash',1)`,
		`INSERT INTO harness_background_permission_manifests(task_id,id,state,payload) VALUES('task-child','manifest','completed',x'7b7d')`,
		`INSERT INTO harness_background_interrupt_stages(task_id,attempt,bindings) VALUES('task-child',1,x'5b5d')`,
		`INSERT INTO harness_background_drain_manifests(task_id,state,payload) VALUES('task-child','completed',x'7b7d')`,
		`INSERT INTO harness_background_execution_failures(task_id,attempt,error,persistence_failure) VALUES('task-child',1,'historical model error',0)`,
	} {
		if _, err := done.native.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed historical graph row: %v", err)
		}
	}
	if _, err := done.native.DB().ExecContext(ctx, `INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload,handled) VALUES('handled',?,'task-child',x'7b7d',1)`, done.binding.ParentSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := done.native.DB().ExecContext(ctx, `CREATE TRIGGER graph_delete_fault BEFORE DELETE ON harness_sessions WHEN OLD.id='task-child/session' BEGIN SELECT RAISE(ABORT,'graph delete fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := done.store.purgeSession(ctx, done.binding.ParentSessionID, false); err == nil {
		t.Fatal("child deletion fault did not roll back graph")
	}
	var count int
	if err := done.native.DB().QueryRowContext(ctx, `SELECT count(*) FROM eino_background_tasks WHERE id=?`, done.binding.TaskID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("task escaped rollback: %d %v", count, err)
	}
	if _, err := done.native.DB().ExecContext(ctx, `DROP TRIGGER graph_delete_fault`); err != nil {
		t.Fatal(err)
	}
	already, _, err := done.store.purgeSession(ctx, done.binding.ParentSessionID, false)
	if err != nil || already {
		t.Fatalf("terminal graph delete: already=%v err=%v", already, err)
	}
	for table := range map[string]bool{"harness_sessions": true, "harness_runs": true, "harness_background_specs": true, "harness_background_bindings": true, "eino_background_tasks": true, "eino_task_events": true, "budget_roots": true, "harness_background_permission_intents": true, "harness_background_permission_grants": true, "harness_background_policy_grants": true, "harness_background_execution_failures": true, "harness_background_inbox": true} {
		if err = done.native.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s remains after graph delete: %d %v", table, count, err)
		}
	}
}

func TestAutomaticRetentionWaitsForBackgroundInbox(t *testing.T) {
	f := completedGraphFixture(t)
	ctx := context.Background()
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := f.native.DB().ExecContext(ctx, `UPDATE harness_sessions SET closed_at=?,updated_at=? WHERE id=?`, old, old, f.binding.ParentSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.DB().ExecContext(ctx, `INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES('unread',?,?,x'7b7d')`, f.binding.ParentSessionID, f.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	if eligible, err := f.store.IsCleanupEligible(ctx, f.binding.ParentSessionID, time.Now().UTC(), policy); err != nil || eligible {
		t.Fatalf("unread graph shown eligible: %v %v", eligible, err)
	}
	service := NewService(f.store, nil, "model")
	if _, err := service.DeleteExpiredSession(ctx, f.binding.ParentSessionID, time.Now().UTC(), policy); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic cleanup removed unread notification: %v", err)
	}
	if _, err := f.native.DB().ExecContext(ctx, `UPDATE harness_background_inbox SET handled=1 WHERE notification_id='unread'`); err != nil {
		t.Fatal(err)
	}
	if eligible, err := f.store.IsCleanupEligible(ctx, f.binding.ParentSessionID, time.Now().UTC(), policy); err != nil || !eligible {
		t.Fatalf("handled graph not eligible: %v %v", eligible, err)
	}
	if removed, err := service.DeleteExpiredSession(ctx, f.binding.ParentSessionID, time.Now().UTC(), policy); err != nil || !removed {
		t.Fatalf("handled terminal graph could not expire: %v", err)
	}
}

func TestExplicitDeleteTerminalGraphMayDiscardUnreadInbox(t *testing.T) {
	f := completedGraphFixture(t)
	ctx := context.Background()
	if _, err := f.native.DB().ExecContext(ctx, `INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES('unread',?,?,x'7b7d')`, f.binding.ParentSessionID, f.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	if already, _, err := f.store.purgeSession(ctx, f.binding.ParentSessionID, false); err != nil || already {
		t.Fatalf("explicit graph deletion: already=%v err=%v", already, err)
	}
}

func TestGraphPersistenceFailureBlocksDeletion(t *testing.T) {
	f := completedGraphFixture(t)
	ctx := context.Background()
	if _, err := f.native.DB().ExecContext(ctx, `INSERT INTO harness_background_execution_failures(task_id,attempt,error,persistence_failure) VALUES(?,1,'uncertain cleanup',1)`, f.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.purgeSession(ctx, f.binding.ParentSessionID, false); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("persistence failure evidence deleted: %v", err)
	}
	var count int
	if err := f.native.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_background_execution_failures WHERE task_id=?`, f.binding.TaskID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure evidence missing: %d %v", count, err)
	}
}

// Assemble a persisted three-level graph through the runtime/ledger seams.
// Enabling delegation in these fixture sessions represents the future role
// factory; it does not assert that model-visible nested tools are wired yet.
func nestedGraphFixture(t *testing.T) (*backgroundHostFixture, []background.Binding) {
	t.Helper()
	f := newBackgroundHostFixture(t)
	ctx := context.Background()
	type saved struct {
		binding background.Binding
		spec    BackgroundExecutionSpec
		scope   background.TaskScope
	}
	var stack []saved
	root := f.binding.ParentSessionID
	for depth := 1; depth <= 3; depth++ {
		if err := f.create(); err != nil {
			t.Fatal(err)
		}
		f.start(t)
		if err := f.begin(f.scope); err != nil {
			t.Fatal(err)
		}
		stack = append(stack, saved{f.binding, f.spec, f.scope})
		if depth == 3 {
			break
		}
		parent, _, err := f.host.Load(ctx, f.binding)
		if err != nil {
			t.Fatal(err)
		}
		parent.Session.Subagents = true
		parent.Session.ConfigVersion++
		if err = f.store.SaveConfig(ctx, parent.Session); err != nil {
			t.Fatal(err)
		}
		for _, event := range []harness.RunEvent{
			{Kind: "tool_start", Status: "pending", Arguments: []byte(`{"instruction":"child goal"}`)},
			{Kind: "tool_execute", Status: "in_progress"},
		} {
			event.SessionID, event.RunID, event.ToolCallID, event.ToolName = parent.Session.ID, parent.RunID, "delegate", "background_agent"
			if _, err = f.store.Append(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		f.spec.Parent = parent.Session
		contract, err := f.spec.Contract()
		if err != nil {
			t.Fatal(err)
		}
		nextID := fmt.Sprintf("nested-%d", depth+1)
		previous := f.binding
		f.binding = background.Binding{TaskID: nextID, ChildSessionID: nextID + "/session", ParentSessionID: parent.Session.ID, RootSessionID: root, ParentTaskID: previous.TaskID, Depth: depth + 1, OriginRunID: parent.RunID, OriginToolCallID: "delegate", Workspace: parent.Session.CWD, ExecutionContract: contract, RootBudgetID: previous.RootBudgetID, AgentVersion: f.spec.AgentVersion, ConfigVersion: parent.Session.ConfigVersion}
		f.scope = background.TaskScope{Binding: f.binding, Attempt: 1}
	}
	bindings := make([]background.Binding, len(stack))
	for i := len(stack) - 1; i >= 0; i-- {
		saved := stack[i]
		bindings[i] = saved.binding
		f.binding, f.spec, f.scope = saved.binding, saved.spec, saved.scope
		if i < len(stack)-1 {
			if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "tool_end", ToolCallID: "delegate", ToolName: "background_agent", Status: "completed"}); err != nil {
				t.Fatal(err)
			}
		}
		task, err := f.tasks.Get(ctx, f.binding.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: root, RunID: f.binding.OriginRunID, Kind: "tool_end", ToolCallID: "delegate", ToolName: "background_agent", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.DB().ExecContext(ctx, `DELETE FROM eino_task_notifications`); err != nil {
		t.Fatal(err)
	}
	return f, bindings
}

func TestDeleteNestedBackgroundGraphChecksEveryDescendant(t *testing.T) {
	f, bindings := nestedGraphFixture(t)
	ctx := context.Background()
	root, leaf := bindings[0].ParentSessionID, bindings[2]
	if _, err := f.native.DB().Exec(`INSERT INTO harness_background_execution_failures(task_id,attempt,error,persistence_failure) VALUES(?,1,'unconfirmed descendant',1)`, leaf.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.purgeSession(ctx, root, false); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("deleted uncertain descendant: %v", err)
	}
	if _, err := f.native.DB().Exec(`DELETE FROM harness_background_execution_failures WHERE task_id=?`, leaf.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.DB().Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES('leaf-unread',?,?,x'7b7d')`, leaf.ParentSessionID, leaf.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.purgeSession(ctx, root, true); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic retention lost unread descendant: %v", err)
	}
	for _, b := range bindings {
		if _, err := f.native.DB().Exec(`INSERT INTO harness_background_stops(task_id,reason,created_at) VALUES(?, 'fixture', 'fixture')`, b.TaskID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewBackgroundInteractionStore(ctx, f.store, &backgroundPermissionAuthority{owner: "owner", session: root}); err != nil {
		t.Fatal(err)
	}
	for _, b := range bindings {
		if _, err := f.native.DB().Exec(`INSERT INTO harness_background_delivery_stages(task_id,attempt,call_id,payload) VALUES(?,1,'old-attempt','{}')`, b.TaskID); err != nil {
			t.Fatal(err)
		}
	}
	if already, _, err := f.store.purgeSession(ctx, root, false); err != nil || already {
		t.Fatalf("recursive explicit delete: %v %v", already, err)
	}
	for _, table := range []string{"harness_sessions", "harness_runs", "harness_background_bindings", "harness_background_tree", "harness_background_stops", "harness_background_specs", "harness_background_inbox", "harness_background_delivery_stages", "budget_roots", "budget_members", "eino_background_tasks"} {
		var count int
		if err := f.native.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("recursive delete retained %s: %d %v", table, count, err)
		}
	}
}
