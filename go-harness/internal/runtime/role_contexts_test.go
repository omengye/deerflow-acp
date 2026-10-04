package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func newRoleContextFixture(t *testing.T) (*executionFixture, *executionAttempt, interaction.RoleContext) {
	t.Helper()
	f := newExecutionFixture(t)
	f.s.BudgetLedger = f.ledger
	s := NewService(f.s, nil, f.req.Session.Model)
	if _, err := s.Coordinator.Attach(f.req.Session.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	controller, err := agents.NewController(f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agents.New(harness.SubagentsConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := registry.Resolve("general-purpose", agents.Parent{Model: f.req.Session.Model})
	if err != nil {
		t.Fatal(err)
	}
	state := interaction.RoleContext{SessionID: f.req.Session.ID, RunID: f.req.RunID,
		Invocation: agents.Invocation{ID: "private-role", RootID: f.req.RootBudgetID, Depth: 1, Spec: spec},
		Messages:   json.RawMessage(`[{"role":"user","content":"private task"},{"role":"assistant","content":"first summary"}]`)}
	if err = controller.Register(context.Background(), state.Invocation, 1, 3); err != nil {
		t.Fatal(err)
	}
	return f, &executionAttempt{service: s, lease: f.lease}, state
}

func assertRoleContextCounts(t *testing.T, f *executionFixture, stages, saved int) {
	t.Helper()
	var actualStages, actualSaved int
	if err := f.s.db.QueryRow(`SELECT (SELECT count(*) FROM harness_role_context_stages),(SELECT count(*) FROM harness_role_contexts)`).Scan(&actualStages, &actualSaved); err != nil || stages != actualStages || saved != actualSaved {
		t.Fatalf("role contexts stages=%d saved=%d want=%d/%d error=%v", actualStages, actualSaved, stages, saved, err)
	}
}

func TestRoleContextCommitIsAtomicWithExecution(t *testing.T) {
	f, a, state := newRoleContextFixture(t)
	ctx := context.Background()
	if err := a.StageRoleContext(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := a.StageRoleContext(ctx, state); err != nil {
		t.Fatal("idempotent callback", err)
	}
	assertRoleContextCounts(t, f, 1, 0)
	if _, err := f.s.db.Exec(`CREATE TRIGGER context_fault BEFORE INSERT ON harness_role_contexts BEGIN SELECT RAISE(ABORT,'fixture context'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err == nil {
		t.Fatal("context write failure was ignored")
	}
	got, err := f.s.Execution(ctx, state.SessionID, state.RunID)
	if err != nil || got.Status != harness.ExecutionRunning {
		t.Fatalf("partial execution commit: %+v %v", got, err)
	}
	assertRoleContextCounts(t, f, 1, 0)
	if _, err = f.s.db.Exec(`DROP TRIGGER context_fault`); err != nil {
		t.Fatal(err)
	}
	result, err := a.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("retry %+v %v", result, err)
	}
	assertRoleContextCounts(t, f, 0, 1)
	if err = a.StageRoleContext(ctx, state); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("finished attempt staged more history: %v", err)
	}
}

func TestRoleContextFailureAndRecoveryDiscardOnlyUncommittedMessages(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		name := "failed"
		if checkpoint {
			name = "interrupted_resume"
		}
		t.Run(name, func(t *testing.T) {
			f, a, state := newRoleContextFixture(t)
			ctx := context.Background()
			if err := a.StageRoleContext(ctx, state); err != nil {
				t.Fatal(err)
			}
			if !checkpoint {
				failure := errors.New("ordinary attempt error")
				if _, err := a.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{}, failure, nil); !errors.Is(err, failure) {
					t.Fatalf("lost original error: %v", err)
				}
				assertRoleContextCounts(t, f, 0, 0)
				return
			}
			f.pending(t)
			cp := f.checkpoint()
			staged := &interaction.StagedExecutionCheckpoint{ID: cp.ID, Data: cp.Data, Interrupts: []interaction.ExecutionInterruptBinding{{IntentID: f.intent.ID, IntentVersion: f.intent.Version, NativeInterruptID: cp.Interrupts[0].NativeInterruptID}}}
			result, err := a.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "waiting_input"}, nil, staged)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
				t.Fatalf("checkpoint %+v %v", result, err)
			}
			assertRoleContextCounts(t, f, 0, 1)
			a.lease = f.resume(t, "owner")
			state.Messages = json.RawMessage(`[{"role":"assistant","content":"uncommitted later history"}]`)
			if err = a.StageRoleContext(ctx, state); err != nil {
				t.Fatal(err)
			}
			assertRoleContextCounts(t, f, 1, 1)
			mustExecutionTx(t, f, func(tx *sql.Tx) error { _, err := f.s.RecoverExecutionsTx(ctx, tx); return err })
			assertRoleContextCounts(t, f, 0, 1)
			mustExecutionTx(t, f, func(tx *sql.Tx) error {
				saved, err := readRoleContextTx(ctx, tx, f.req.RootBudgetID, state.Invocation.ID, 0)
				if err == nil && (saved.Revision != 1 || string(saved.Context.Messages) == string(state.Messages)) {
					t.Fatal("recovery published unsaved messages")
				}
				return err
			})
			if err = a.StageRoleContext(ctx, state); !errors.Is(err, harness.ErrExecutionConflict) {
				t.Fatalf("old attempt remained writable: %v", err)
			}
		})
	}
}

func TestRoleContextEligibilityDistinguishesMissingCorruptAndStorageFailure(t *testing.T) {
	f, a, state := newRoleContextFixture(t)
	ctx := context.Background()
	b := background.Binding{TaskID: "leaf", ParentSessionID: f.req.Session.ID, RootSessionID: f.req.Session.ID, RootBudgetID: f.req.RootBudgetID, ParentInvocationID: state.Invocation.ID, Depth: 2}
	spec := BackgroundExecutionSpec{Version: 5, Automatic: &AutomaticContinuationPolicy{Enabled: true, Limit: 3}}
	n := bt.Notification{ID: "leaf-result", TaskID: b.TaskID, SessionID: b.ParentSessionID, Kind: bt.NotificationCompleted, Version: 1}
	check := func(want string, storageFailure bool) {
		t.Helper()
		err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
			got, _, err := notificationEligibilityTx(ctx, tx, b, spec, n)
			if err == nil && got != want {
				t.Fatalf("eligibility %q want %q", got, want)
			}
			return err
		})
		if (err != nil) != storageFailure {
			t.Fatalf("storageFailure=%v error=%v", storageFailure, err)
		}
	}
	check("waiting_parent", false)
	if err := a.StageRoleContext(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err := a.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	check("queued", false)
	if _, err := f.s.db.Exec(`UPDATE harness_role_contexts SET payload='{}'`); err != nil {
		t.Fatal(err)
	}
	check("blocked", false)
	original, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE harness_role_contexts SET payload=?`, original); err != nil {
		t.Fatal(err)
	}
	for _, invalidRecipe := range []string{`{}`, `{`} {
		if _, err = f.s.db.Exec(`UPDATE harness_agent_invocations SET spec_json=?`, invalidRecipe); err != nil {
			t.Fatal(err)
		}
		check("blocked", false)
	}
	if _, err := f.s.db.Exec(`DROP TABLE harness_role_contexts`); err != nil {
		t.Fatal(err)
	}
	check("", true)
}

func newRoleResultFixture(t *testing.T) (*executionFixture, *executionAttempt) {
	t.Helper()
	ctx := context.Background()
	var invocation agents.Invocation
	f := newExecutionWaitFixture(t, func(child *backgroundHostFixture) {
		depth := 2
		config := harness.SubagentsConfig{MaxDepth: &depth, CustomAgents: map[string]harness.AgentConfig{
			"parent": {Description: "private parent", SystemPrompt: "complete task", DelegateTo: []string{"general-purpose"}},
		}}
		catalog, err := agents.Snapshot(&config)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := agents.New(config, nil)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := registry.Resolve("parent", agents.Parent{Model: child.spec.Parent.Model})
		if err != nil {
			t.Fatal(err)
		}
		role, err := registry.Resolve("general-purpose", agents.Parent{Model: parent.Model, Tools: parent.Tools, Skills: parent.Skills, MaxTurns: parent.MaxTurns, TimeoutSeconds: parent.TimeoutSeconds})
		if err != nil {
			t.Fatal(err)
		}
		invocation = agents.Invocation{ID: "private-parent", RootID: child.binding.RootBudgetID, Depth: 1, Spec: parent}
		controller, err := agents.NewController(child.store.db)
		if err != nil {
			t.Fatal(err)
		}
		if err = controller.Register(ctx, invocation, 1, 3); err != nil {
			t.Fatal(err)
		}
		child.spec.Version, child.spec.Depth, child.spec.ParentInvocationID = 5, depth, invocation.ID
		child.spec.Roles, child.spec.Role = catalog, &role
		child.binding.Depth, child.binding.ParentInvocationID = depth, invocation.ID
		child.binding.ExecutionContract, err = child.spec.Contract()
		if err != nil {
			t.Fatal(err)
		}
		child.scope.Binding = child.binding
	})
	s := automaticService(t, f, &automaticWaitEngine{})
	s.ContinuationHostPolicy = f.child.spec.HostPolicy
	a := &executionAttempt{service: s, lease: f.lease}
	state := interaction.RoleContext{SessionID: f.req.Session.ID, RunID: f.req.RunID, Invocation: invocation, Messages: json.RawMessage(`[{"role":"user","content":"private task"},{"role":"assistant","content":"waiting"}]`)}
	if err := a.StageRoleContext(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.complete(t)
	task, err := f.child.tasks.Get(ctx, f.child.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	n := bt.Notification{ID: "child-completed", TaskID: task.Spec.ID, SessionID: f.req.Session.ID, Kind: bt.NotificationCompleted, Version: task.Version, CreatedAt: time.Now()}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, n.ID, n.SessionID, n.TaskID, data); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		req, lease, _, err := s.beginContinuationTx(ctx, tx, "owner", n.SessionID, n.ID)
		if err == nil {
			a.lease = lease
			state.RunID = req.RunID
		}
		return err
	})
	state.Messages = json.RawMessage(`[{"role":"user","content":"private task"},{"role":"assistant","content":"final parent summary"}]`)
	if err = a.StageRoleContext(ctx, state); err != nil {
		t.Fatal(err)
	}
	return f.executionFixture, a
}

func TestRoleResultPublicationRollsBackContextExecutionAndInboxTogether(t *testing.T) {
	for _, table := range []string{"harness_role_results", "harness_background_inbox"} {
		t.Run(table, func(t *testing.T) {
			f, a := newRoleResultFixture(t)
			ctx := context.Background()
			if _, err := f.s.db.Exec(`CREATE TRIGGER result_fault BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'fixture result'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := a.service.finishExecutionAttempt(ctx, a.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err == nil {
				t.Fatal("publication failure was ignored")
			}
			assertRoleContextCounts(t, f, 1, 1)
			got, err := f.s.Execution(ctx, a.lease.Scope.SessionID, a.lease.Scope.MemberID)
			if err != nil || got.Status != harness.ExecutionRunning {
				t.Fatalf("partial terminal state: %+v %v", got, err)
			}
			var results, notes int
			if err = f.s.db.QueryRow(`SELECT (SELECT count(*) FROM harness_role_results),(SELECT count(*) FROM harness_background_inbox WHERE notification_id LIKE 'role-result/%')`).Scan(&results, &notes); err != nil || results != 0 || notes != 0 {
				t.Fatalf("partial result publication: %d %d %v", results, notes, err)
			}
			if _, err = f.s.db.Exec(`DROP TRIGGER result_fault`); err != nil {
				t.Fatal(err)
			}
			result, err := a.service.finishExecutionAttempt(ctx, a.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("retry result %+v %v", result, err)
			}
			assertRoleContextCounts(t, f, 0, 2)
			if err = f.s.db.QueryRow(`SELECT (SELECT count(*) FROM harness_role_results),(SELECT count(*) FROM harness_background_inbox WHERE notification_id LIKE 'role-result/%')`).Scan(&results, &notes); err != nil || results != 1 || notes != 1 {
				t.Fatalf("result not published exactly once: %d %d %v", results, notes, err)
			}
		})
	}
}

func TestRoleResultOrdinaryFailureCannotHideOtherErrorsOrUncertainEffects(t *testing.T) {
	for _, failure := range []string{"provider", "storage", "uncertain"} {
		t.Run(failure, func(t *testing.T) {
			f, a := newRoleResultFixture(t)
			ctx := context.Background()
			var runErr error = &interaction.RoleContinuationFailure{Cause: errors.New("private provider detail")}
			if failure == "storage" {
				runErr = errors.Join(runErr, errors.New("storage failed"))
			}
			if failure == "uncertain" {
				for _, kind := range []string{"tool_start", "tool_execute"} {
					status := "pending"
					if kind == "tool_execute" {
						status = "in_progress"
					}
					if _, err := f.s.Append(ctx, harness.RunEvent{SessionID: a.lease.Scope.SessionID, RunID: a.lease.Scope.MemberID, Kind: kind, Status: status, ToolName: "write_file", ToolCallID: "unconfirmed", Arguments: json.RawMessage(`{"path":"file"}`)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := a.service.finishExecutionAttempt(ctx, a.lease, harness.RunResult{}, runErr, nil)
			wantStatus, wantResults := harness.ExecutionNeedsReconciliation, 0
			if failure == "provider" {
				wantStatus, wantResults = harness.ExecutionFailed, 1
			}
			if result.Execution == nil || result.Execution.Status != wantStatus || (err == nil) != (failure == "provider") {
				t.Fatalf("failure %s: %+v %v", failure, result, err)
			}
			assertRoleContextCounts(t, f, 0, 1)
			var count int
			if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_role_results`).Scan(&count); err != nil || count != wantResults {
				t.Fatalf("unsafe result propagation %d %v", count, err)
			}
		})
	}
}

func TestRoleResultIsBoundToItsCommittedConversationRevision(t *testing.T) {
	f, a := newRoleResultFixture(t)
	ctx := context.Background()
	if _, err := a.service.finishExecutionAttempt(ctx, a.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	validate := func(tx *sql.Tx) error {
		n, err := readContinuationNotification(ctx, tx, a.lease.Scope.SessionID, "role-result/"+a.lease.Scope.MemberID)
		if err != nil {
			return err
		}
		b, spec, _, _, err := readContinuationOrigin(ctx, tx, n.SessionID, n.TaskID)
		if err != nil {
			return err
		}
		result, err := readRoleResultTx(ctx, tx, b, spec, n)
		if err == nil && (result.ContextRevision != 2 || result.Result != "final parent summary") {
			t.Fatalf("summary not pinned: %+v", result)
		}
		return err
	}
	mustExecutionTx(t, f, validate)
	if _, err := f.s.db.Exec(`UPDATE harness_role_contexts SET payload='{}' WHERE role_id='private-parent' AND revision=2`); err != nil {
		t.Fatal(err)
	}
	if err := executionTx(ctx, f.s, validate); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("changed completed conversation was accepted: %v", err)
	}
}
