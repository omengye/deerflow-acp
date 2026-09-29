package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type backgroundPermissionAuthority struct{ owner, session string }

func (a *backgroundPermissionAuthority) AuthorizeTaskAccess(_ context.Context, actor harness.TaskActor) error {
	if actor.OwnerID != a.owner || actor.SessionID != a.session {
		return harness.ErrNotAttached
	}
	return nil
}

type backgroundPermissionGrantKey struct{}
type backgroundPermissionFixture struct {
	*backgroundHostFixture
	permissions *BackgroundInteractionStore
	authority   *backgroundPermissionAuthority
	attempt     *BackgroundInteractionAttempt
	requests    []harness.PermissionRequest
	intents     []interaction.PermissionIntent
	state       *harness.BackgroundInteraction
}

func newBackgroundPermissionFixture(t *testing.T, configure ...func(*backgroundHostFixture)) *backgroundPermissionFixture {
	t.Helper()
	ctx := context.Background()
	host := newBackgroundHostFixture(t)
	for _, apply := range configure {
		apply(host)
	}
	if err := host.create(); err != nil {
		t.Fatal(err)
	}
	f := &backgroundPermissionFixture{backgroundHostFixture: host, authority: &backgroundPermissionAuthority{owner: "owner", session: host.binding.ParentSessionID}}
	var err error
	f.permissions, err = NewBackgroundInteractionStore(ctx, host.store, f.authority)
	if err != nil {
		t.Fatal(err)
	}
	f.tasks = f.native.Tasks().WithHooks(sqlite.TaskHooks{Transition: func(ctx context.Context, tx *sql.Tx, before, after *bt.Task) error {
		scope := background.TaskScope{Binding: f.binding, Attempt: before.Attempt}
		if before.Status == bt.StatusPending && after.Status == bt.StatusRunning {
			scope.Attempt = after.Attempt
			if _, err := tx.ExecContext(ctx, `INSERT INTO harness_background_child_leases VALUES(?,?,?)`, f.binding.ChildSessionID, f.binding.TaskID, after.Attempt); err != nil {
				return err
			}
			return f.permissions.TransitionTx(ctx, tx, scope, before, after)
		}
		if before.Status == bt.StatusWaitingInput && after.Status == bt.StatusPending {
			grant, ok := ctx.Value(backgroundPermissionGrantKey{}).(background.ResumeGrant)
			if !ok {
				return harness.ErrPermissionDenied
			}
			if err := f.permissions.CommitResumeTx(ctx, tx, f.actor(), f.binding, grant); err != nil {
				return err
			}
		}
		if before.Status == bt.StatusRunning && after.Status != bt.StatusRunning {
			if err := f.host.CommitAttemptTx(ctx, tx, scope, string(after.Status), f.adapter.CommitAttemptTx); err != nil {
				return err
			}
			if after.Status == bt.StatusWaitingInput || after.Status == bt.StatusSuspended {
				if err := f.native.SetCheckpointTx(ctx, tx, f.binding.TaskID+"/checkpoint", []byte("opaque native runner checkpoint")); err != nil {
					return err
				}
			}
			if err := f.permissions.TransitionTx(ctx, tx, scope, before, after); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM harness_background_child_leases WHERE child_session_id=?`, f.binding.ChildSessionID)
			return err
		}
		if err := f.host.TransitionTx(ctx, tx, scope, before, after); err != nil {
			return err
		}
		return f.permissions.TransitionTx(ctx, tx, scope, before, after)
	}})
	f.open(t)
	return f
}
func (f *backgroundPermissionFixture) actor() harness.TaskActor {
	return harness.TaskActor{OwnerID: f.authority.owner, SessionID: f.binding.ParentSessionID}
}
func (f *backgroundPermissionFixture) fence(ctx context.Context, tx *sql.Tx, scope background.TaskScope, allowStopping bool) error {
	if scope.Binding != f.binding {
		return harness.ErrTaskOriginConflict
	}
	if err := f.native.Tasks().CheckAttemptTx(ctx, tx, scope.Binding.TaskID, scope.Attempt, allowStopping); err != nil {
		return err
	}
	var taskID string
	var attempt int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id,attempt FROM harness_background_child_leases WHERE child_session_id=?`, scope.Binding.ChildSessionID).Scan(&taskID, &attempt); err != nil {
		return err
	}
	if taskID != scope.Binding.TaskID || attempt != scope.Attempt {
		return bt.ErrLeaseLost
	}
	return nil
}
func (f *backgroundPermissionFixture) open(t *testing.T) {
	t.Helper()
	f.start(t)
	if err := f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	var err error
	f.attempt, err = f.permissions.OpenAttempt(context.Background(), f.scope, f.fence)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *backgroundPermissionFixture) pending(t *testing.T, call string) {
	t.Helper()
	ctx := context.Background()
	p := harness.PermissionRequest{ID: f.binding.TaskID + "/" + call, SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, ConfigVersion: f.binding.ConfigVersion, ToolCallID: call, ToolName: "write_file", Arguments: json.RawMessage("{ \"path\": \"report.txt\",\n \"content\": \"private content\" }")}
	if err := f.attempt.Publish(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: call, ToolName: p.ToolName, Status: "pending", Arguments: p.Arguments}); err != nil {
		t.Fatal(err)
	}
	intent, err := f.attempt.PreparePermission(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	f.requests = append(f.requests, p)
	f.intents = append(f.intents, intent)
	if err = f.attempt.StageInterrupts(ctx, []interaction.ExecutionInterruptBinding{{IntentID: intent.ID, IntentVersion: intent.Version, NativeInterruptID: "native/" + call}}); err != nil {
		t.Fatal(err)
	}
}
func (f *backgroundPermissionFixture) wait(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		return err
	}
	_, err = f.tasks.WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: task.Version, Checkpoint: []byte(`{"sequence":1,"waiting":true}`)})
	if err != nil {
		return err
	}
	f.state, err = f.permissions.Interaction(ctx, f.actor(), f.binding)
	return err
}
func (f *backgroundPermissionFixture) approval(decision harness.PermissionDecision) harness.TaskApproval {
	return harness.TaskApproval{ID: f.state.ID, TaskVersion: f.state.TaskVersion, Decision: decision}
}
func (f *backgroundPermissionFixture) resume(request harness.TaskApproval) error {
	ctx := context.Background()
	grant, err := f.permissions.PrepareResume(ctx, f.actor(), f.binding, request)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, backgroundPermissionGrantKey{}, grant)
	_, err = f.tasks.Resume(ctx, &bt.ResumeRequest{TaskID: f.binding.TaskID, ExpectedVersion: request.TaskVersion, Data: grant.Data})
	return err
}

func TestBackgroundPermissionParallelNativeTargetsAndOneUseGrants(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	ctx := context.Background()
	f.pending(t, "one")
	f.pending(t, "two")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	if f.state == nil || !f.state.Resumable || len(f.state.WaitingInputs) != 2 {
		t.Fatal("pending public state", f.state)
	}
	data, _ := json.Marshal(f.state)
	if strings.Contains(string(data), "native/") || strings.Contains(string(data), "private content") {
		t.Fatal("private checkpoint/arguments leaked")
	}
	request := f.approval("")
	request.Evidence = []byte(`{"decisions":[{"intentId":"task-child/one","version":1,"decision":"allow_once"},{"intentId":"task-child/two","version":1,"decision":"reject_once"}]}`)
	if err := f.resume(request); err != nil {
		t.Fatal(err)
	}
	old := f.attempt
	f.open(t)
	if f.scope.Attempt != 2 {
		t.Fatal("native attempt reused")
	}
	for i, p := range f.requests {
		target := f.attempt.Hooks().Targets["native/"+p.ToolCallID]
		decision, err := f.attempt.ResolvePermission(ctx, p, f.intents[i], target.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = old.ResolvePermission(ctx, p, f.intents[i], target.GrantID); !errors.Is(err, bt.ErrLeaseLost) {
			t.Fatal("old attempt authorized", err)
		}
		if decision == harness.AllowOnce {
			if err = f.attempt.Publish(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "in_progress"}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.attempt.ResolvePermission(ctx, p, f.intents[i], target.GrantID); !errors.Is(err, harness.ErrExecutionConflict) {
				t.Fatal("consumed grant replayed", err)
			}
			if err = f.attempt.Publish(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "completed"}); err != nil {
				t.Fatal(err)
			}
		} else if err = f.attempt.Publish(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	var consumed int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants WHERE state='consumed'`).Scan(&consumed); err != nil || consumed != 2 {
		t.Fatal("grant consumption", consumed, err)
	}
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, "completed", "completed")
}

func TestBackgroundPermissionManifestAndApprovalRollbackAtomically(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	ctx := context.Background()
	f.pending(t, "one")
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_manifest BEFORE INSERT ON harness_background_permission_manifests BEGIN SELECT RAISE(ABORT,'manifest fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.wait(t); err == nil {
		t.Fatal("manifest write failure accepted")
	}
	f.assertState(t, "running", "")
	var checkpoints int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, f.binding.TaskID+"/checkpoint").Scan(&checkpoints); err != nil || checkpoints != 0 {
		t.Fatal("orphan promoted checkpoint")
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_manifest`); err != nil {
		t.Fatal(err)
	}
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_resume BEFORE UPDATE ON eino_background_tasks WHEN OLD.status='waiting_input' AND NEW.status='pending' BEGIN SELECT RAISE(ABORT,'resume fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); err == nil {
		t.Fatal("native resume write failure accepted")
	}
	var grants int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("approval partially committed")
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_resume`); err != nil {
		t.Fatal(err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_receipt BEFORE UPDATE ON harness_tool_receipts WHEN NEW.state='started' AND NEW.run_id='task-child' BEGIN SELECT RAISE(ABORT,'receipt fault'); END`); err != nil {
		t.Fatal(err)
	}
	p := f.requests[0]
	event := harness.RunEvent{Kind: "tool_execute", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "in_progress"}
	if err := f.attempt.Publish(ctx, event); err == nil {
		t.Fatal("failed receipt authorized dispatch")
	}
	target := f.attempt.Hooks().Targets["native/one"]
	if decision, err := f.attempt.ResolvePermission(ctx, p, f.intents[0], target.GrantID); err != nil || decision != harness.AllowOnce {
		t.Fatal("failed receipt consumed grant", decision, err)
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := f.attempt.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundPermissionRejectsClientTargetsAndStaleOwner(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	ctx := context.Background()
	f.pending(t, "one")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []string{`{"native/one":{"GrantID":"forged"}}`, `{"decisions":[{"intentId":"task-child/one","version":1,"decision":"allow_once","nativeTarget":"forged"}]}`, `{"decisions":[{"intentId":"task-child/other","version":1,"decision":"allow_once"}]}`, `{"decisions":[{"intentId":"task-child/one","version":1,"decision":"allow_once"},{"intentId":"task-child/one","version":1,"decision":"reject_once"}]}`} {
		request := f.approval("")
		request.Evidence = []byte(evidence)
		if _, err := f.permissions.PrepareResume(ctx, f.actor(), f.binding, request); err == nil {
			t.Fatal("client target/evidence accepted", evidence)
		}
	}
	old := f.actor()
	f.authority.owner = "reconnected"
	if _, err := f.permissions.PrepareResume(ctx, old, f.binding, f.approval(harness.AllowOnce)); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal("old owner approved", err)
	}
	if err := f.resume(f.approval(harness.RejectOnce)); err != nil {
		t.Fatal(err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); !errors.Is(err, bt.ErrVersionConflict) {
		t.Fatal("stale task version replayed", err)
	}
}

func TestBackgroundPermissionDriftBlocksApprovedDispatch(t *testing.T) {
	for _, kind := range []string{"checkpoint", "native", "receipt", "input", "policy"} {
		t.Run(kind, func(t *testing.T) {
			f := newBackgroundPermissionFixture(t)
			f.pending(t, "one")
			if err := f.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := f.resume(f.approval(harness.AllowOnce)); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "checkpoint":
				_, err = f.native.DB().Exec(`UPDATE eino_checkpoints SET payload='changed' WHERE id=?`, f.binding.TaskID+"/checkpoint")
			case "native":
				_, err = f.native.DB().Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,'later','message','{}')`, f.binding.ChildSessionID)
			case "receipt":
				_, err = f.native.DB().Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, f.binding.TaskID)
			case "input":
				_, err = f.native.DB().Exec(`UPDATE harness_inputs SET content='[]' WHERE run_id=?`, f.binding.TaskID)
			case "policy":
				_, err = f.native.DB().Exec(`UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`, f.binding.ParentSessionID)
			}
			if err != nil {
				t.Fatal(err)
			}
			task, err := f.tasks.Get(context.Background(), f.binding.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.tasks.Start(context.Background(), &bt.StartTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: task.Version}); err == nil {
				t.Fatal("stale checkpoint authorized new attempt")
			}
		})
	}
}

func TestBackgroundPermissionMissingParallelBindingRollsBackWait(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	f.pending(t, "one")
	f.pending(t, "two")
	onlyOne, err := json.Marshal([]interaction.ExecutionInterruptBinding{{IntentID: f.intents[0].ID, IntentVersion: f.intents[0].Version, NativeInterruptID: "native/one"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`UPDATE harness_background_interrupt_stages SET bindings=? WHERE task_id=?`, onlyOne, f.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	if err = f.wait(t); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("checkpoint with an omitted pending tool was promoted", err)
	}
	f.assertState(t, "running", "")
	var count int
	if err = f.native.DB().QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, f.binding.TaskID+"/checkpoint").Scan(&count); err != nil || count != 0 {
		t.Fatal("incomplete wait left a promoted checkpoint", count, err)
	}
	if err = f.attempt.StageInterrupts(context.Background(), []interaction.ExecutionInterruptBinding{{IntentID: f.intents[1].ID, IntentVersion: f.intents[1].Version, NativeInterruptID: "native/two"}}); err != nil {
		t.Fatal(err)
	}
	if err = f.wait(t); err != nil || f.state == nil || len(f.state.WaitingInputs) != 2 || !f.state.Resumable {
		t.Fatal("complete native targets could not wait", f.state, err)
	}
}

func TestBackgroundPermissionIdleCancellationRevokesAtomically(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting", true: "approved_pending"}[approved], func(t *testing.T) {
			ctx := context.Background()
			f := newBackgroundPermissionFixture(t)
			f.pending(t, "one")
			if err := f.wait(t); err != nil {
				t.Fatal(err)
			}
			approval := f.approval(harness.AllowOnce)
			if approved {
				if err := f.resume(approval); err != nil {
					t.Fatal(err)
				}
			}
			task, err := f.tasks.Get(ctx, f.binding.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			cancel := func() error {
				_, err := f.tasks.RequestCancel(ctx, &bt.RequestCancelRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Reason: "cancel waiting work"})
				return err
			}
			if _, err = f.native.DB().Exec(`CREATE TRIGGER fail_broker_cancel BEFORE UPDATE ON harness_background_permission_manifests WHEN NEW.state='closed' BEGIN SELECT RAISE(ABORT,'cancel broker fault'); END`); err != nil {
				t.Fatal(err)
			}
			if err = cancel(); err == nil {
				t.Fatal("broker failure accepted cancellation")
			}
			unchanged, err := f.tasks.Get(ctx, task.Spec.ID)
			if err != nil || unchanged.Status != task.Status || unchanged.Version != task.Version {
				t.Fatal("broker failure partly cancelled native task", unchanged, err)
			}
			var pending, ready int
			if err = f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_intents WHERE task_id=? AND state='pending'`, task.Spec.ID).Scan(&pending); err != nil || pending != 1 {
				t.Fatal("failed cancel changed pending intent", pending, err)
			}
			if err = f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants WHERE task_id=? AND state='ready'`, task.Spec.ID).Scan(&ready); err != nil || ready != map[bool]int{true: 1, false: 0}[approved] {
				t.Fatal("failed cancel revoked ready grant", ready, err)
			}
			if _, err = f.native.DB().Exec(`DROP TRIGGER fail_broker_cancel`); err != nil {
				t.Fatal(err)
			}
			if err = cancel(); err != nil {
				t.Fatal(err)
			}
			f.assertState(t, "cancelled", "waiting_input")
			var state string
			if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_manifests WHERE task_id=?`, task.Spec.ID).Scan(&state); err != nil || state != "closed" {
				t.Fatal("cancel did not close manifest", state, err)
			}
			if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_intents WHERE task_id=?`, task.Spec.ID).Scan(&state); err != nil || state != "cancelled" {
				t.Fatal("cancel did not revoke pending intent", state, err)
			}
			if approved {
				if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_grants WHERE task_id=?`, task.Spec.ID).Scan(&state); err != nil || state != "revoked" {
					t.Fatal("cancel did not revoke ready grant", state, err)
				}
			}
			if state, err := f.permissions.Interaction(ctx, f.actor(), f.binding); err != nil || state != nil {
				t.Fatal("cancelled task exposes a resumable interaction", state, err)
			}
			if _, err := f.permissions.PrepareResume(ctx, f.actor(), f.binding, approval); !errors.Is(err, bt.ErrVersionConflict) {
				t.Fatal("old approval survived cancellation", err)
			}
		})
	}
}
