package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type backgroundTargetFixture struct {
	owner, child *backgroundHostFixture
	recipient    interaction.RoleContext
	note         bt.Notification
	service      *Service
}

// The scan uses the durable task/inbox stores directly. Embedding the
// controller makes an unexpected management call fail instead of succeeding
// against an unrelated native service.
type continuationScanController struct{ harness.BackgroundController }

func newBackgroundTargetFixture(t *testing.T, foreground bool) *backgroundTargetFixture {
	t.Helper()
	ctx := context.Background()
	owner := newBackgroundHostFixture(t)
	depth, enabled := 3, true
	config := harness.SubagentsConfig{MaxDepth: &depth, AutoContinue: &enabled,
		Agents: map[string]harness.AgentConfig{"general-purpose": {DelegateTo: []string{"middle", "leaf"}}},
		CustomAgents: map[string]harness.AgentConfig{
			"middle": {Description: "private middle", SystemPrompt: "middle policy", DelegateTo: []string{"leaf"}},
			"leaf":   {Description: "private leaf", SystemPrompt: "leaf policy", Tools: []string{}},
		}}
	registry, err := agents.New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentSpec, err := registry.Resolve("general-purpose", agents.Parent{Model: owner.spec.Parent.Model})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agents.Snapshot(&config)
	if err != nil {
		t.Fatal(err)
	}
	owner.spec.Version, owner.spec.Depth, owner.spec.Role, owner.spec.Roles = 5, 1, &parentSpec, catalog
	owner.spec.Automatic = ContinuationPolicy(catalog)
	owner.binding.Depth = 1
	owner.binding.ExecutionContract, err = owner.spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	owner.scope.Binding = owner.binding
	controller, err := agents.NewController(owner.store.db)
	if err != nil {
		t.Fatal(err)
	}
	invocation := agents.Invocation{ID: "background/" + owner.binding.TaskID, RootID: owner.binding.RootBudgetID, Depth: 1, Spec: parentSpec}
	if err = controller.Register(ctx, invocation, 1, 3); err != nil {
		t.Fatal(err)
	}
	if err = owner.create(); err != nil {
		t.Fatal(err)
	}
	owner.start(t)
	if err = owner.begin(owner.scope); err != nil {
		t.Fatal(err)
	}
	request, _, err := owner.host.Load(ctx, owner.binding)
	if err != nil {
		t.Fatal(err)
	}
	if foreground {
		spec, err := registry.Resolve("middle", agents.Parent{Model: parentSpec.Model, Tools: parentSpec.Tools, Skills: parentSpec.Skills, MaxTurns: parentSpec.MaxTurns, TimeoutSeconds: parentSpec.TimeoutSeconds})
		if err != nil {
			t.Fatal(err)
		}
		invocation = agents.Invocation{ID: "private-middle", RootID: invocation.RootID, ParentID: invocation.ID, Depth: 2, Spec: spec}
		if err = controller.Register(ctx, invocation, 1, 3); err != nil {
			t.Fatal(err)
		}
	}
	leafSpec, err := registry.Resolve("leaf", agents.Parent{Model: invocation.Spec.Model, Tools: invocation.Spec.Tools, Skills: invocation.Spec.Skills, MaxTurns: invocation.Spec.MaxTurns, TimeoutSeconds: invocation.Spec.TimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	child := &backgroundHostFixture{store: owner.store, native: owner.native, host: owner.host, adapter: owner.adapter,
		spec: BackgroundExecutionSpec{Version: 5, Parent: request.Session, AgentVersion: owner.spec.AgentVersion, HostPolicy: owner.spec.HostPolicy,
			Input: []harness.Content{{Type: "text", Text: "leaf goal"}}, Extension: json.RawMessage(`{}`), OriginArguments: `{"instruction":"leaf goal","role":"leaf"}`,
			Role: &leafSpec, Roles: catalog, Depth: invocation.Depth + 1, ParentTaskID: owner.binding.TaskID, ParentInvocationID: invocation.ID, Automatic: ContinuationPolicy(catalog)}}
	contract, err := child.spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	child.binding = background.Binding{TaskID: "leaf", ParentSessionID: request.Session.ID, ChildSessionID: "leaf/session", OriginRunID: request.RunID, OriginToolCallID: "delegate-leaf", Workspace: request.Session.CWD,
		ExecutionContract: contract, RootBudgetID: request.RootBudgetID, AgentVersion: child.spec.AgentVersion, ConfigVersion: request.Session.ConfigVersion,
		RootSessionID: owner.binding.RootSession(), ParentTaskID: owner.binding.TaskID, ParentInvocationID: invocation.ID, Depth: child.spec.Depth}
	child.scope = background.TaskScope{Binding: child.binding, Attempt: 1}
	child.installTaskHooks()
	for _, kind := range []string{"tool_start", "tool_execute"} {
		status := "pending"
		if kind == "tool_execute" {
			status = "in_progress"
		}
		if _, err = owner.store.Append(ctx, harness.RunEvent{SessionID: request.Session.ID, RunID: request.RunID, Kind: kind, Status: status, ToolName: "background_agent", ToolCallID: "delegate-leaf", Arguments: json.RawMessage(child.spec.OriginArguments)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = child.create(); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.store.Append(ctx, harness.RunEvent{SessionID: request.Session.ID, RunID: request.RunID, Kind: "tool_end", Status: "completed", ToolName: "background_agent", ToolCallID: "delegate-leaf"}); err != nil {
		t.Fatal(err)
	}
	child.start(t)
	if err = child.begin(child.scope); err != nil {
		t.Fatal(err)
	}
	task, err := child.tasks.Get(ctx, child.binding.TaskID)
	if err == nil {
		task, err = child.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
	}
	if err != nil {
		t.Fatal(err)
	}
	note := bt.Notification{ID: "leaf-done", TaskID: task.Spec.ID, SessionID: request.Session.ID, Kind: bt.NotificationCompleted, Version: task.Version, CreatedAt: time.Now()}
	data, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.store.db.Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, note.ID, note.SessionID, note.TaskID, data); err != nil {
		t.Fatal(err)
	}
	service := NewService(owner.store, &automaticWaitEngine{}, owner.spec.Parent.Model)
	service.ContinuationHostPolicy = owner.spec.HostPolicy
	if _, err = service.Coordinator.Attach(owner.binding.RootSession(), "owner"); err != nil {
		t.Fatal(err)
	}
	service.Background = continuationScanController{}
	f := &backgroundTargetFixture{owner: owner, child: child, service: service, note: note,
		recipient: interaction.RoleContext{Invocation: invocation, SessionID: request.Session.ID, RunID: request.RunID, Messages: json.RawMessage(`[{"role":"user","content":"private goal"},{"role":"assistant","content":"delegated work"}]`)}}
	if err = executionTx(ctx, owner.store, func(tx *sql.Tx) error {
		return stageRoleContextTx(ctx, tx, background.BudgetScope(owner.scope), f.recipient)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *backgroundTargetFixture) completeParent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	native, err := f.owner.tasks.Get(ctx, f.owner.binding.TaskID)
	if err == nil {
		_, err = f.owner.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: native.Spec.ID, ExpectedVersion: native.Version})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (f *backgroundTargetFixture) target() (target backgroundContinuationTarget, err error) {
	err = executionTx(context.Background(), f.owner.store, func(tx *sql.Tx) error {
		target, err = backgroundContinuationTargetTx(context.Background(), tx, f.child.binding, f.child.spec)
		return err
	})
	return
}

func TestBackgroundContinuationTargetRequiresCommittedOriginalHost(t *testing.T) {
	for _, foreground := range []bool{false, true} {
		name := "background_parent"
		if foreground {
			name = "private_foreground_parent_in_background_host"
		}
		t.Run(name, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, foreground)
			if _, err := f.target(); !errors.Is(err, errRoleConversationUnavailable) {
				t.Fatalf("live parent reused as continuation host: %v", err)
			}
			f.completeParent(t)
			target, err := f.target()
			if err != nil || target.Role.Context.Invocation.ID != f.recipient.Invocation.ID || target.Role.Revision != 1 || target.Host != f.owner.binding || target.HostVersion < 2 || string(target.Role.Context.Messages) != string(f.recipient.Messages) {
				t.Fatalf("recipient reconstruction: %+v %v", target, err)
			}
			err = executionTx(context.Background(), f.owner.store, func(tx *sql.Tx) error {
				return validateBackgroundContinuationTargetTx(context.Background(), tx, f.child.binding, f.child.spec, target)
			})
			if err != nil {
				t.Fatal(err)
			}
			target.HostVersion++
			err = executionTx(context.Background(), f.owner.store, func(tx *sql.Tx) error {
				return validateBackgroundContinuationTargetTx(context.Background(), tx, f.child.binding, f.child.spec, target)
			})
			if !errors.Is(err, harness.ErrExecutionUnresumable) {
				t.Fatalf("changed host version accepted: %v", err)
			}
		})
	}
}

func TestBackgroundContinuationScanFindsPrivateRecipientWithoutRootDelivery(t *testing.T) {
	f := newBackgroundTargetFixture(t, true)
	ctx := context.Background()
	root := f.owner.binding.RootSession()
	for _, completed := range []bool{false, true} {
		if completed {
			f.completeParent(t)
			if _, err := f.owner.store.db.Exec(`UPDATE harness_notification_work SET next_at=0 WHERE notification_id=?`, f.note.ID); err != nil {
				t.Fatal(err)
			}
		}
		id, err := f.service.nextAutomaticNotification(ctx, "owner", root)
		if err != nil || id != "" {
			t.Fatalf("private notification injected into root: %q %v", id, err)
		}
		var state, session, recipient, detail string
		if err = f.owner.store.db.QueryRow(`SELECT state,session_id,recipient_role,detail FROM harness_notification_work WHERE notification_id=?`, f.note.ID).Scan(&state, &session, &recipient, &detail); err != nil || state != "waiting_parent" || session != f.child.binding.ParentSessionID || recipient != f.recipient.Invocation.ID {
			t.Fatalf("lost private owner: %q %q %q %v", state, session, recipient, err)
		}
		if strings.Contains(detail, "context is saved") != completed {
			t.Fatalf("host readiness not rechecked: %q", detail)
		}
	}
	if _, err := f.service.nextAutomaticNotification(ctx, "other-owner", root); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("stale owner inspected private state: %v", err)
	}
}

func TestBackgroundContinuationTargetRejectsChangedStateAndPreservesStorageErrors(t *testing.T) {
	for _, mode := range []string{"snapshot", "foreign_host", "policy", "uncertain", "stopped", "database"} {
		t.Run(mode, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, true)
			f.completeParent(t)
			var query string
			var args []any
			switch mode {
			case "snapshot":
				query = `UPDATE harness_role_contexts SET payload='{}'`
			case "foreign_host":
				query, args = `UPDATE harness_agent_invocations SET parent_id='' WHERE id=?`, []any{f.recipient.Invocation.ID}
			case "policy":
				query, args = `UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`, []any{f.owner.binding.RootSession()}
			case "uncertain":
				query, args = `UPDATE harness_tool_receipts SET state='uncertain' WHERE run_id=? AND tool_call_id='delegate-leaf'`, []any{f.owner.binding.TaskID}
			case "stopped":
				query, args = `INSERT INTO harness_background_stops VALUES(?,'stop','fixture')`, []any{f.owner.binding.TaskID}
			case "database":
				query = `DROP TABLE harness_role_contexts`
			}
			if _, err := f.owner.store.db.Exec(query, args...); err != nil {
				t.Fatal(err)
			}
			_, err := f.target()
			if err == nil {
				t.Fatal("invalid target accepted")
			}
			if mode == "stopped" {
				if !errors.Is(err, errBackgroundParentStopped) {
					t.Fatal(err)
				}
			} else if mode == "database" {
				if blockedContinuationSource(err) || errors.Is(err, errRoleConversationUnavailable) {
					t.Fatalf("database failure was hidden: %v", err)
				}
			} else if !blockedContinuationSource(err) {
				t.Fatalf("invalid source was not classified: %v", err)
			}
		})
	}
}

func (f *backgroundTargetFixture) rootNotification(t *testing.T) bt.Notification {
	t.Helper()
	f.completeParent(t)
	task, err := f.owner.tasks.Get(context.Background(), f.owner.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return bt.Notification{ID: "root-result", TaskID: task.Spec.ID, SessionID: f.owner.binding.RootSession(), Kind: bt.NotificationCompleted, Version: task.Version, CreatedAt: time.Now()}
}

func (f *backgroundTargetFixture) insertNotification(t *testing.T, n bt.Notification) {
	t.Helper()
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.store.db.Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, n.ID, n.SessionID, n.TaskID, data); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundContinuationScanDoesNotStarveRootAfterPrivatePage(t *testing.T) {
	f := newBackgroundTargetFixture(t, true)
	note := f.note
	// The fixture already has the first private notification. Fill one page.
	for i := 1; i < 100; i++ {
		note.ID = fmt.Sprintf("private-%d", i)
		f.insertNotification(t, note)
	}
	root := f.rootNotification(t)
	f.insertNotification(t, root)
	ctx := context.Background()
	if id, err := f.service.nextAutomaticNotification(ctx, "owner", root.SessionID); err != nil || id != "" {
		t.Fatalf("private page became root input: %q %v", id, err)
	}
	// Simulate a slow scanner: all first-page waits are due again. A retry
	// delay alone cannot provide fairness under this timing.
	if _, err := f.owner.store.db.Exec(`UPDATE harness_notification_work SET next_at=1`); err != nil {
		t.Fatal(err)
	}
	if id, err := f.service.nextAutomaticNotification(ctx, "owner", root.SessionID); err != nil || id != root.ID {
		t.Fatalf("private waits starved a ready root: %q %v", id, err)
	}
}

func TestBackgroundContinuationScanQuarantinesCorruptionAndSurfacesStorageFailure(t *testing.T) {
	for _, mode := range []string{"notification", "context", "origin", "database"} {
		t.Run(mode, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, true)
			root := f.rootNotification(t)
			f.insertNotification(t, root)
			query, args := "", []any{}
			switch mode {
			case "notification":
				query, args = `UPDATE harness_background_inbox SET payload='{}' WHERE notification_id=?`, []any{f.note.ID}
			case "context":
				query = `UPDATE harness_role_contexts SET payload='{}'`
			case "origin":
				query, args = `UPDATE harness_background_specs SET payload='{}' WHERE task_id=?`, []any{f.child.binding.TaskID}
			case "database":
				query = `DROP TABLE harness_role_contexts`
			}
			if _, err := f.owner.store.db.Exec(query, args...); err != nil {
				t.Fatal(err)
			}
			id, err := f.service.nextAutomaticNotification(context.Background(), "owner", root.SessionID)
			if mode == "database" {
				if err == nil || id != "" || blockedContinuationSource(err) {
					t.Fatalf("storage error hidden: %q %v", id, err)
				}
				return
			}
			if err != nil || id != root.ID {
				t.Fatalf("corrupt private notification prevented root delivery: %q %v", id, err)
			}
			var state, session string
			if err = f.owner.store.db.QueryRow(`SELECT state,session_id FROM harness_notification_work WHERE notification_id=?`, f.note.ID).Scan(&state, &session); err != nil || state != "blocked" || session != f.note.SessionID {
				t.Fatalf("corrupt private source not quarantined: %q %q %v", state, session, err)
			}
		})
	}
}

func TestBackgroundContinuationScanPersistsRootRetryTime(t *testing.T) {
	f := newBackgroundTargetFixture(t, false)
	root := f.rootNotification(t)
	f.insertNotification(t, root)
	next := time.Now().Add(time.Hour).UnixNano()
	if _, err := f.owner.store.db.Exec(`INSERT INTO harness_automatic_continuations(root_id,session_id,policy_digest,max_runs,used,next_at) VALUES(?,?,?,?,0,?)`, f.owner.binding.RootBudgetID, root.SessionID, f.owner.spec.Roles.Digest, f.owner.spec.Automatic.Limit, next); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if id, err := f.service.nextAutomaticNotification(ctx, "owner", root.SessionID); err != nil || id != "" {
		t.Fatalf("root retry bypassed: %q %v", id, err)
	}
	var state string
	var retry int64
	if err := f.owner.store.db.QueryRow(`SELECT state,next_at FROM harness_notification_work WHERE notification_id=?`, root.ID).Scan(&state, &retry); err != nil || state != "queued" || retry != next {
		t.Fatalf("root backoff missing: %q %d %v", state, retry, err)
	}
	// Advance both durable clocks without sleeping or relying on scan speed.
	for _, table := range []string{"harness_notification_work", "harness_automatic_continuations"} {
		if _, err := f.owner.store.db.Exec(`UPDATE ` + table + ` SET next_at=1`); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := f.service.nextAutomaticNotification(ctx, "owner", root.SessionID); err != nil || id != root.ID {
		t.Fatalf("due root was not revisited: %q %v", id, err)
	}
}
