package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestBackgroundHostSpecSharesNativeCreationTransaction(t *testing.T) {
	ctx := context.Background()
	s, native, ledger, parent := budgetService(t, harness.DefaultBudgetLimits(), budget.Config{})
	parent.Subagents = true
	parent.ConfigVersion++
	if err := s.Store.SaveConfig(ctx, parent); err != nil {
		t.Fatal(err)
	}
	origin := harness.RunRequest{Session: parent, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "parent goal"}}}
	origin.RootBudgetID = origin.RunID
	if err := s.Store.BeginRun(ctx, origin); err != nil {
		t.Fatal(err)
	}
	for _, event := range []harness.RunEvent{
		{SessionID: parent.ID, RunID: origin.RunID, Kind: "tool_start", ToolCallID: "delegate", ToolName: "background_agent", Status: "pending", Arguments: []byte(`{"instruction":"child goal"}`)},
		{SessionID: parent.ID, RunID: origin.RunID, Kind: "tool_execute", ToolCallID: "delegate", ToolName: "background_agent", Status: "in_progress"},
	} {
		if _, err := s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	host, err := NewBackgroundExecutionStore(ctx, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	spec := BackgroundExecutionSpec{Version: 1, Parent: parent, AgentVersion: "deerflow-child-v1", HostPolicy: "host-policy", Input: []harness.Content{{Type: "text", Text: "child goal"}}, Extension: []byte(`{"version":1,"skills":[],"sandboxPolicy":"disabled"}`), OriginArguments: `{"instruction":"child goal"}`}
	contract, err := spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	binding := background.Binding{TaskID: "task-child", ParentSessionID: parent.ID, ChildSessionID: "child-session", OriginRunID: origin.RunID, OriginToolCallID: "delegate", Workspace: parent.CWD, ExecutionContract: contract, RootBudgetID: origin.RunID, AgentVersion: spec.AgentVersion, ConfigVersion: parent.ConfigVersion}
	adapter, err := background.NewLedgerAdapter(native, ledger)
	if err != nil {
		t.Fatal(err)
	}
	tasks := native.Tasks().WithHooks(sqlite.TaskHooks{Create: func(ctx context.Context, tx *sql.Tx, _ *bt.Task) error {
		if err := host.BindTx(ctx, tx, binding, spec); err != nil {
			return err
		}
		return adapter.BindTaskTx(ctx, tx, binding)
	}})
	request := &bt.CreateTaskRequest{Spec: bt.Spec{ID: binding.TaskID, ExecutorKey: "fixture", SessionID: parent.ID, Description: "child", NotifySession: true}, LeaseExpiryPolicy: bt.LeaseExpiryFail}
	if _, err := native.DB().Exec(`CREATE TRIGGER fail_host_spec BEFORE INSERT ON harness_background_specs BEGIN SELECT RAISE(ABORT,'host spec failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(ctx, request); err == nil {
		t.Fatal("native create should fail with host transaction")
	}
	for _, table := range []string{"harness_background_specs", "eino_background_tasks"} {
		var count int
		if err := native.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	for _, table := range []string{"harness_sessions", "harness_runs", "harness_inputs", "budget_members"} {
		var count int
		if err := native.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("partial child in %s count=%d err=%v", table, count, err)
		}
	}
	if _, err := native.DB().Exec(`DROP TRIGGER fail_host_spec`); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	child, saved, err := host.Load(ctx, binding)
	if err != nil || child.Session.ID != binding.ChildSessionID || child.Session.Subagents || child.RunID != binding.TaskID || child.RootBudgetID != origin.RunID || len(child.Input) != 1 || child.Input[0].Text != "child goal" {
		t.Fatalf("child=%+v spec=%+v err=%v", child, saved, err)
	}
	listed, err := s.Store.List(ctx, parent.CWD, "", 100)
	if err != nil || len(listed) != 1 || listed[0].ID != parent.ID {
		t.Fatalf("child exposed in foreground listing: %+v %v", listed, err)
	}
	foreground := NewService(s.Store, nil, "test")
	if _, err := foreground.Load(ctx, "intruder", child.Session.ID, parent.CWD, false, nil); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("child loaded as foreground: %v", err)
	}
	if err := foreground.Coordinator.Authorize(child.Session.ID, "intruder"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("failed child load acquired attachment: %v", err)
	}
	var rootID, memberSession string
	if err := native.DB().QueryRow(`SELECT root_id,session_id FROM budget_members WHERE id=?`, binding.TaskID).Scan(&rootID, &memberSession); err != nil || rootID != origin.RunID || memberSession != child.Session.ID {
		t.Fatalf("budget root=%s session=%s err=%v", rootID, memberSession, err)
	}
	tx, err := native.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = host.BindTx(ctx, tx, binding, spec); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.Input = []harness.Content{{Type: "text", Text: "changed goal"}}
	changed.OriginArguments = `{"instruction":"changed goal"}`
	changedBinding := binding
	changedBinding.ExecutionContract, _ = changed.Contract()
	tx, err = native.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = host.BindTx(ctx, tx, changedBinding, changed)
	_ = tx.Rollback()
	if !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatalf("changed intent: %v", err)
	}
	if changedBinding.ExecutionContract != binding.ExecutionContract {
		t.Fatal("new instruction changed immutable execution policy")
	}
	scope := background.TaskScope{Binding: binding, Attempt: 1}
	check := func(ctx context.Context, tx *sql.Tx, scope background.TaskScope) error {
		return native.Tasks().CheckAttemptTx(ctx, tx, scope.Binding.TaskID, scope.Attempt, false)
	}
	if err := host.PinEngineContract(ctx, scope, "engine-v1", check); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("unclaimed attempt pinned contract: %v", err)
	}
	if _, err := native.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: binding.TaskID, ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := host.PinEngineContract(ctx, scope, "engine-v1", check); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.PinEngineContract(ctx, scope, "changed-engine", check); !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*background.Binding){func(b *background.Binding) { b.ChildSessionID = "different-child" }, func(b *background.Binding) { b.RootBudgetID = "different-root" }, func(b *background.Binding) { b.OriginRunID = "different-run" }} {
		wrong := binding
		mutate(&wrong)
		if _, _, err := host.Load(ctx, wrong); !errors.Is(err, harness.ErrTaskOriginConflict) {
			t.Fatalf("forged binding accepted %+v: %v", wrong, err)
		}
	}
	metadataChange := spec
	metadataChange.Parent.Title = "new title"
	metadataChange.Parent.UpdatedAt = metadataChange.Parent.UpdatedAt.Add(100)
	if c, err := metadataChange.Contract(); err != nil || c != contract {
		t.Fatalf("incidental metadata changed contract: %s %v", c, err)
	}
	parent.ConfigVersion++
	parent.Subagents = false
	if err := s.Store.SaveConfig(ctx, parent); err != nil {
		t.Fatal(err)
	}
	tx, err = native.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = host.CheckParentPolicyTx(ctx, tx, binding, spec)
	_ = tx.Rollback()
	if !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("revoked parent: %v", err)
	}
	child.Session.Model = "different-model"
	if err := s.Store.SaveConfig(ctx, child.Session); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.Load(ctx, binding); !errors.Is(err, harness.ErrTaskOriginConflict) {
		t.Fatalf("changed child config: %v", err)
	}
}

type backgroundHostFixture struct {
	store   *Store
	native  *sqlite.Store
	host    *BackgroundExecutionStore
	adapter *background.LedgerAdapter
	tasks   *sqlite.TaskStore
	spec    BackgroundExecutionSpec
	binding background.Binding
	scope   background.TaskScope
}

// The fixture drives the native SQL provider directly to inject faults between
// ledger and business writes. These hooks supply the same durable binding and
// child lease as Service; Service's worker/cleanup behavior has separate tests.
func newBackgroundHostFixture(t *testing.T) *backgroundHostFixture {
	t.Helper()
	ctx := context.Background()
	s, native, ledger, parent := budgetService(t, harness.DefaultBudgetLimits(), budget.Config{})
	parent.Subagents = true
	parent.ConfigVersion++
	if err := s.Store.SaveConfig(ctx, parent); err != nil {
		t.Fatal(err)
	}
	origin := harness.RunRequest{Session: parent, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "parent goal"}}}
	origin.RootBudgetID = origin.RunID
	if err := s.Store.BeginRun(ctx, origin); err != nil {
		t.Fatal(err)
	}
	for _, event := range []harness.RunEvent{
		{SessionID: parent.ID, RunID: origin.RunID, Kind: "tool_start", ToolCallID: "delegate", ToolName: "background_agent", Status: "pending", Arguments: []byte(`{"instruction":"child goal"}`)},
		{SessionID: parent.ID, RunID: origin.RunID, Kind: "tool_execute", ToolCallID: "delegate", ToolName: "background_agent", Status: "in_progress"},
	} {
		if _, err := s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := background.New(ctx, background.Config{Store: native}); err != nil {
		t.Fatal(err)
	}
	host, err := NewBackgroundExecutionStore(ctx, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := background.NewLedgerAdapter(native, ledger)
	if err != nil {
		t.Fatal(err)
	}
	f := &backgroundHostFixture{store: s.Store, native: native, host: host, adapter: adapter,
		spec: BackgroundExecutionSpec{Version: 1, Parent: parent, AgentVersion: "child-v1", HostPolicy: "host-v1", Input: []harness.Content{{Type: "text", Text: "child goal"}}, Extension: []byte(`{}`), OriginArguments: `{"instruction":"child goal"}`}}
	contract, err := f.spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	f.binding = background.Binding{TaskID: "task-child", ChildSessionID: "task-child/session", ParentSessionID: parent.ID, OriginRunID: origin.RunID, OriginToolCallID: "delegate", Workspace: parent.CWD, ExecutionContract: contract, RootBudgetID: origin.RunID, AgentVersion: f.spec.AgentVersion, ConfigVersion: parent.ConfigVersion}
	f.scope = background.TaskScope{Binding: f.binding, Attempt: 1}
	f.tasks = native.Tasks().WithHooks(sqlite.TaskHooks{
		Create: func(ctx context.Context, tx *sql.Tx, _ *bt.Task) error {
			data, _ := json.Marshal(f.binding)
			b := f.binding
			if _, err := tx.ExecContext(ctx, `INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES(?,?,?,?,?,?,?)`, b.TaskID, b.ParentSessionID, b.ChildSessionID, b.OriginRunID, b.OriginToolCallID, b.IntentHash, data); err != nil {
				return err
			}
			if err := host.BindTx(ctx, tx, f.binding, f.spec); err != nil {
				return err
			}
			return adapter.BindTaskTx(ctx, tx, f.binding)
		},
		Transition: func(ctx context.Context, tx *sql.Tx, before, after *bt.Task) error {
			if before.Status == bt.StatusPending && after.Status == bt.StatusRunning {
				_, err := tx.ExecContext(ctx, `INSERT INTO harness_background_child_leases VALUES(?,?,?)`, f.binding.ChildSessionID, f.binding.TaskID, after.Attempt)
				return err
			}
			if before.Status != bt.StatusRunning || after.Status == bt.StatusRunning {
				return host.TransitionTx(ctx, tx, background.TaskScope{Binding: f.binding, Attempt: before.Attempt}, before, after)
			}
			scope := background.TaskScope{Binding: f.binding, Attempt: before.Attempt}
			if err := host.CommitAttemptTx(ctx, tx, scope, string(after.Status), adapter.CommitAttemptTx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM harness_background_child_leases WHERE child_session_id=?`, f.binding.ChildSessionID)
			return err
		},
	})
	return f
}

func (f *backgroundHostFixture) create() error {
	_, err := f.tasks.Create(context.Background(), &bt.CreateTaskRequest{Spec: bt.Spec{ID: f.binding.TaskID, ExecutorKey: "fixture", SessionID: f.binding.ParentSessionID, NotifySession: true}, LeaseExpiryPolicy: bt.LeaseExpiryFail})
	return err
}

func (f *backgroundHostFixture) start(t *testing.T) {
	t.Helper()
	task, err := f.tasks.Get(context.Background(), f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = f.tasks.Start(context.Background(), &bt.StartTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: task.Version})
	if err != nil {
		t.Fatal(err)
	}
	f.scope.Attempt = task.Attempt
}

func (f *backgroundHostFixture) begin(scope background.TaskScope) error {
	ctx := context.Background()
	tx, err := f.native.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f.host.BeforeAttemptTx(ctx, tx, scope, f.adapter.BeforeAttemptTx); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *backgroundHostFixture) receipt(t *testing.T, call, tool string, started bool) harness.ToolReceipt {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "tool_start", ToolCallID: call, ToolName: tool, Status: "pending", Arguments: []byte(`{"path":"file"}`)}); err != nil {
		t.Fatal(err)
	}
	if started {
		if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "tool_execute", ToolCallID: call, ToolName: tool, Status: "in_progress"}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := readReceipt(ctx, f.native.DB(), f.binding.ChildSessionID, f.binding.TaskID, call)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *backgroundHostFixture) assertState(t *testing.T, wantRun, wantLedger string) {
	t.Helper()
	var status string
	if err := f.native.DB().QueryRow(`SELECT status FROM harness_runs WHERE id=?`, f.binding.TaskID).Scan(&status); err != nil || status != wantRun {
		t.Fatalf("business status=%s want=%s err=%v", status, wantRun, err)
	}
	var outcome string
	if err := f.native.DB().QueryRow(`SELECT outcome FROM budget_attempts WHERE id=?`, background.BudgetScope(f.scope).AttemptID).Scan(&outcome); err != nil || outcome != wantLedger {
		t.Fatalf("ledger outcome=%s want=%s err=%v", outcome, wantLedger, err)
	}
}

func TestBackgroundHostFirstBindRequiresExactReceipt(t *testing.T) {
	for _, mismatch := range []string{"raw_digest", "config_version"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newBackgroundHostFixture(t)
			if mismatch == "raw_digest" {
				f.spec.OriginArguments = `{ "instruction": "child goal" }`
			} else {
				r, err := readReceipt(context.Background(), f.native.DB(), f.binding.ParentSessionID, f.binding.OriginRunID, f.binding.OriginToolCallID)
				if err != nil {
					t.Fatal(err)
				}
				r.ConfigVersion++
				data, _ := json.Marshal(r)
				if _, err := f.native.DB().Exec(`UPDATE harness_tool_receipts SET receipt=? WHERE run_id=?`, data, r.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.create(); !errors.Is(err, harness.ErrReceiptConflict) {
				t.Fatalf("receipt mismatch accepted: %v", err)
			}
			for _, table := range []string{"harness_background_specs", "harness_background_bindings", "eino_background_tasks", "eino_task_notifications"} {
				var count int
				if err := f.native.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s retained %d rows: %v", table, count, err)
				}
			}
			for _, table := range []string{"harness_sessions", "harness_runs", "harness_inputs", "budget_members"} {
				var count int
				if err := f.native.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s retained partial child %d: %v", table, count, err)
				}
			}
		})
	}
}

func TestBackgroundHostAdmissionIsFencedAndAtomic(t *testing.T) {
	f := newBackgroundHostFixture(t)
	if err := f.create(); err != nil {
		t.Fatal(err)
	}
	if err := f.begin(f.scope); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("unclaimed attempt: %v", err)
	}
	f.start(t)
	for _, mutate := range []func(*background.TaskScope){func(s *background.TaskScope) { s.Attempt++ }, func(s *background.TaskScope) { s.Binding.ChildSessionID = "foreign" }} {
		bad := f.scope
		mutate(&bad)
		if err := f.begin(bad); err == nil {
			t.Fatalf("accepted forged scope %+v", bad)
		}
	}
	if _, err := f.native.DB().Exec(`UPDATE harness_background_child_leases SET attempt=2`); err != nil {
		t.Fatal(err)
	}
	if err := f.begin(f.scope); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("lost child lease: %v", err)
	}
	if _, err := f.native.DB().Exec(`UPDATE harness_background_child_leases SET attempt=1; CREATE TRIGGER fail_running BEFORE UPDATE ON harness_runs WHEN NEW.status='running' BEGIN SELECT RAISE(ABORT,'projection fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.begin(f.scope); err == nil {
		t.Fatal("business fault should roll ledger back")
	}
	var count int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM budget_attempts WHERE member_id=?`, f.binding.TaskID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial ledger admission: %d %v", count, err)
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_running`); err != nil {
		t.Fatal(err)
	}
	if err := f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, "running", "")
	f.receipt(t, "read", "read_file", false)
}

func TestBackgroundHostWaitingPreservesReceiptsAcrossAttempts(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundHostFixture(t)
	if err := f.create(); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if err := f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	before := f.receipt(t, "write", "write_file", false)
	if _, err := f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2}); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("completed with pending receipt: %v", err)
	}
	f.assertState(t, "running", "")
	paused, err := f.tasks.WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("checkpoint")})
	if err != nil {
		t.Fatal(err)
	}
	f.assertState(t, "waiting_input", "waiting_input")
	after, err := readReceipt(ctx, f.native.DB(), before.SessionID, before.RunID, before.ToolCallID)
	if err != nil || after.State != before.State || after.Version != before.Version || after.ArgumentsDigest != before.ArgumentsDigest {
		t.Fatalf("pause changed pending receipt: %+v %v", after, err)
	}
	if _, err = f.tasks.Resume(ctx, &bt.ResumeRequest{TaskID: f.binding.TaskID, ExpectedVersion: paused.Version, Data: []byte(`{"approved":true}`)}); err != nil {
		t.Fatal(err)
	}
	oldScope := f.scope
	f.start(t)
	if err = f.begin(oldScope); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stale attempt admitted: %v", err)
	}
	if err = f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	for _, event := range []harness.RunEvent{
		{SessionID: before.SessionID, RunID: before.RunID, Kind: "tool_execute", ToolCallID: "write", ToolName: "write_file", Status: "in_progress"},
		{SessionID: before.SessionID, RunID: before.RunID, Kind: "tool_end", ToolCallID: "write", ToolName: "write_file", Status: "completed"},
	} {
		if _, err = f.store.Append(ctx, event); err != nil {
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
	f.assertState(t, "completed", "completed")
	var starts int
	if err = f.native.DB().QueryRow(`SELECT count(*) FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='tool_start'`, f.binding.TaskID).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("repeated tool start count=%d: %v", starts, err)
	}
}

func TestBackgroundHostRejectsUnsafeWaitingAndCompletion(t *testing.T) {
	for _, receiptState := range []string{"started", "uncertain"} {
		t.Run(receiptState, func(t *testing.T) {
			f := newBackgroundHostFixture(t)
			ctx := context.Background()
			if err := f.create(); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			if err := f.begin(f.scope); err != nil {
				t.Fatal(err)
			}
			f.receipt(t, "write", "write_file", true)
			if receiptState == "uncertain" {
				if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "tool_end", ToolCallID: "write", ToolName: "write_file", Status: "failed", Content: []harness.Content{{Type: "text", Text: "lost response"}}}); err != nil {
					t.Fatal(err)
				}
			}
			for _, operation := range []func() (*bt.Task, error){
				func() (*bt.Task, error) {
					return f.tasks.WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("unsafe")})
				},
				func() (*bt.Task, error) {
					return f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("unsafe")})
				},
				func() (*bt.Task, error) {
					return f.tasks.Yield(ctx, &bt.YieldTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2})
				},
				func() (*bt.Task, error) {
					return f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2})
				},
			} {
				if _, err := operation(); !errors.Is(err, harness.ErrReceiptConflict) {
					t.Fatalf("unsafe projection accepted: %v", err)
				}
				f.assertState(t, "running", "")
			}
		})
	}
}

func TestBackgroundHostTerminalReceiptAndLedgerRollback(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		for _, fault := range []string{"receipt", "event", "business", "none"} {
			t.Run(outcome+"/"+fault, func(t *testing.T) {
				ctx := context.Background()
				f := newBackgroundHostFixture(t)
				if err := f.create(); err != nil {
					t.Fatal(err)
				}
				f.start(t)
				if err := f.begin(f.scope); err != nil {
					t.Fatal(err)
				}
				f.receipt(t, "pending", "write_file", false)
				f.receipt(t, "write", "write_file", true)
				f.receipt(t, "read", "read_file", true)
				// Revocation blocks future effects but cannot block final evidence.
				if _, err := f.native.DB().Exec(`UPDATE harness_session_configs SET subagents=0,version=version+1 WHERE session_id=?`, f.binding.ParentSessionID); err != nil {
					t.Fatal(err)
				}
				trigger := map[string]string{
					"receipt":  `CREATE TRIGGER projection_fault BEFORE UPDATE ON harness_tool_receipts BEGIN SELECT RAISE(ABORT,'receipt fault'); END`,
					"event":    `CREATE TRIGGER projection_fault BEFORE INSERT ON harness_events BEGIN SELECT RAISE(ABORT,'event fault'); END`,
					"business": `CREATE TRIGGER projection_fault BEFORE UPDATE ON harness_runs BEGIN SELECT RAISE(ABORT,'business fault'); END`,
				}[fault]
				if trigger != "" {
					if _, err := f.native.DB().Exec(trigger); err != nil {
						t.Fatal(err)
					}
				}
				finish := func() error {
					if outcome == "failed" {
						_, err := f.tasks.Fail(ctx, &bt.FailTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Error: "provider stopped"})
						return err
					}
					_, err := f.tasks.AckCancel(ctx, &bt.AckCancelRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Reason: "user cancelled"})
					return err
				}
				if trigger != "" {
					if err := finish(); err == nil {
						t.Fatal("fault did not abort native transition")
					}
					f.assertState(t, "running", "")
					for call, want := range map[string]harness.ReceiptState{"pending": harness.ReceiptPending, "write": harness.ReceiptStarted, "read": harness.ReceiptStarted} {
						r, err := readReceipt(ctx, f.native.DB(), f.binding.ChildSessionID, f.binding.TaskID, call)
						if err != nil || r.State != want {
							t.Fatalf("partial receipt %s: %+v %v", call, r, err)
						}
					}
					var ends int
					if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='tool_end'`, f.binding.TaskID).Scan(&ends); err != nil || ends != 0 {
						t.Fatalf("partial terminal events=%d: %v", ends, err)
					}
					if _, err := f.native.DB().Exec(`DROP TRIGGER projection_fault`); err != nil {
						t.Fatal(err)
					}
				}
				if err := finish(); err != nil {
					t.Fatal(err)
				}
				f.assertState(t, outcome, outcome)
				for call, want := range map[string]harness.ReceiptState{"pending": harness.ReceiptNotExecuted, "write": harness.ReceiptUncertain, "read": harness.ReceiptNoEffect} {
					r, err := readReceipt(ctx, f.native.DB(), f.binding.ChildSessionID, f.binding.TaskID, call)
					if err != nil || r.State != want || r.Error == "" {
						t.Fatalf("unsettled terminal receipt %s: %+v %v", call, r, err)
					}
				}
			})
		}
	}
}

func TestBackgroundHostIdleCancellationProjectsWithoutEndingBudgetAgain(t *testing.T) {
	for _, from := range []string{"pending", "waiting_input", "suspended"} {
		t.Run(from, func(t *testing.T) {
			ctx := context.Background()
			f := newBackgroundHostFixture(t)
			if err := f.create(); err != nil {
				t.Fatal(err)
			}
			if from != "pending" {
				f.start(t)
				if err := f.begin(f.scope); err != nil {
					t.Fatal(err)
				}
				f.receipt(t, "approve", "write_file", false)
				var err error
				if from == "waiting_input" {
					_, err = f.tasks.WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("checkpoint")})
				} else {
					_, err = f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("checkpoint")})
				}
				if err != nil {
					t.Fatal(err)
				}
				f.assertState(t, "waiting_input", "waiting_input")
			}
			task, err := f.tasks.Get(ctx, f.binding.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			cancel := func() error {
				_, err := f.tasks.RequestCancel(ctx, &bt.RequestCancelRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Reason: "user cancelled idle task"})
				return err
			}
			if _, err = f.native.DB().Exec(`CREATE TRIGGER idle_cancel_fault BEFORE UPDATE ON harness_runs BEGIN SELECT RAISE(ABORT,'idle projection fault'); END`); err != nil {
				t.Fatal(err)
			}
			if err = cancel(); err == nil {
				t.Fatal("idle projection fault did not roll back")
			}
			unchanged, err := f.tasks.Get(ctx, task.Spec.ID)
			if err != nil || unchanged.Status != task.Status || unchanged.Version != task.Version {
				t.Fatalf("partial idle cancellation: %+v %v", unchanged, err)
			}
			if from != "pending" {
				r, err := readReceipt(ctx, f.native.DB(), f.binding.ChildSessionID, f.binding.TaskID, "approve")
				if err != nil || r.State != harness.ReceiptPending || r.Version != 1 {
					t.Fatalf("idle rollback lost pending receipt: %+v %v", r, err)
				}
			}
			if _, err = f.native.DB().Exec(`DROP TRIGGER idle_cancel_fault`); err != nil {
				t.Fatal(err)
			}
			if err = cancel(); err != nil {
				t.Fatal(err)
			}
			if from != "pending" {
				f.assertState(t, "cancelled", "waiting_input")
				r, err := readReceipt(ctx, f.native.DB(), f.binding.ChildSessionID, f.binding.TaskID, "approve")
				if err != nil || r.State != harness.ReceiptNotExecuted || r.Version != 2 {
					t.Fatalf("idle cancellation left pending receipt: %+v %v", r, err)
				}
			} else {
				var status string
				var attempts int
				if err := f.native.DB().QueryRow(`SELECT status FROM harness_runs WHERE id=?`, f.binding.TaskID).Scan(&status); err != nil || status != "cancelled" {
					t.Fatalf("idle pending status=%s: %v", status, err)
				}
				if err := f.native.DB().QueryRow(`SELECT count(*) FROM budget_attempts WHERE member_id=?`, f.binding.TaskID).Scan(&attempts); err != nil || attempts != 0 {
					t.Fatalf("idle cancellation created budget attempt=%d: %v", attempts, err)
				}
			}
		})
	}
}

func TestBackgroundHostEffectPolicyUsesDurableSpecAndAllowsEndedParent(t *testing.T) {
	for name, mutation := range map[string]string{
		"model":          `UPDATE harness_sessions SET model='different' WHERE id=?`,
		"cwd":            `UPDATE harness_sessions SET cwd='different' WHERE id=?`,
		"mode":           `UPDATE harness_sessions SET mode='plan' WHERE id=?`,
		"approval":       `UPDATE harness_session_configs SET approval_mode='never' WHERE session_id=?`,
		"version":        `UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`,
		"subagents":      `UPDATE harness_session_configs SET subagents=1 WHERE session_id=?`,
		"parent_revoked": `UPDATE harness_session_configs SET subagents=0 WHERE session_id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newBackgroundHostFixture(t)
			ctx := context.Background()
			if err := f.create(); err != nil {
				t.Fatal(err)
			}
			check := func(scope background.TaskScope) error {
				tx, err := f.native.DB().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				return f.host.CheckAttemptPolicyTx(ctx, tx, scope)
			}
			if err := check(f.scope); err != nil {
				t.Fatal(err)
			}
			bad := f.scope
			bad.Binding.RootBudgetID = "foreign-root"
			if err := check(bad); !errors.Is(err, harness.ErrTaskOriginConflict) {
				t.Fatalf("forged policy binding: %v", err)
			}
			// The submit tool's final receipt and the foreground turn may end
			// while an explicitly detached child still has valid host policy.
			if _, err := f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ParentSessionID, RunID: f.binding.OriginRunID, Kind: "tool_end", ToolCallID: f.binding.OriginToolCallID, ToolName: "background_agent", Status: "completed"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.DB().Exec(`UPDATE harness_runs SET status='completed' WHERE id=?`, f.binding.OriginRunID); err != nil {
				t.Fatal(err)
			}
			if err := check(f.scope); err != nil {
				t.Fatalf("ended origin revoked unchanged child policy: %v", err)
			}
			id, want := f.binding.ChildSessionID, harness.ErrTaskOriginConflict
			if name == "parent_revoked" {
				id, want = f.binding.ParentSessionID, harness.ErrPermissionDenied
			}
			if _, err := f.native.DB().Exec(mutation, id); err != nil {
				t.Fatal(err)
			}
			if err := check(f.scope); !errors.Is(err, want) {
				t.Fatalf("policy drift accepted: %v want %v", err, want)
			}
		})
	}
}

func TestBackgroundHostSpecRejectsForeignAssetsAndUnboundOrigins(t *testing.T) {
	ctx := context.Background()
	s, _, _, parent := budgetService(t, harness.BudgetLimits{}, budget.Config{})
	host, err := NewBackgroundExecutionStore(ctx, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	spec := BackgroundExecutionSpec{Version: 1, Parent: parent, AgentVersion: "agent-v1", HostPolicy: "host-v1", Input: []harness.Content{{Type: "image", Asset: &harness.AssetRef{SessionID: parent.ID}}}, Extension: []byte(`{}`)}
	if _, err := spec.Contract(); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal(err)
	}
	spec.Input = []harness.Content{{Type: "text", Text: "goal"}}
	spec.OriginArguments = `{"instruction":"goal"}`
	contract, err := spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	binding := background.Binding{TaskID: "task", ParentSessionID: parent.ID, ChildSessionID: "child", OriginRunID: "nonexistent", OriginToolCallID: "call", Workspace: parent.CWD, ExecutionContract: contract, RootBudgetID: "root", AgentVersion: spec.AgentVersion, ConfigVersion: parent.ConfigVersion}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = host.BindTx(ctx, tx, binding, spec)
	_ = tx.Rollback()
	if !errors.Is(err, harness.ErrNotFound) {
		t.Fatal(err)
	}
}
