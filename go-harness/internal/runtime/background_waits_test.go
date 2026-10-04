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

// Descendant execution is driven by the native store here; SDK fixtures cover
// the real role factory, worker, checkpoint and model/tool execution paths.
func addWaitTarget(t *testing.T, f *backgroundPermissionFixture, id string) *bt.Task {
	t.Helper()
	ctx := context.Background()
	task, err := f.native.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: id, SessionID: f.binding.ChildSessionID, ExecutorKey: "fixture"}, LeaseExpiryPolicy: bt.LeaseExpiryFail})
	if err != nil {
		t.Fatal(err)
	}
	b := f.binding
	b.TaskID, b.ChildSessionID, b.ParentSessionID = id, id+"/session", f.binding.ChildSessionID
	b.RootSessionID, b.ParentTaskID, b.Depth = f.binding.RootSession(), f.binding.TaskID, 2
	b.OriginRunID, b.OriginToolCallID = f.binding.TaskID, "submit-"+id
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES(?,?,?,?,?,?,?)`, b.TaskID, b.ParentSessionID, b.ChildSessionID, b.OriginRunID, b.OriginToolCallID, b.IntentHash, data); err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`INSERT INTO harness_background_tree(task_id,root_session_id,parent_task_id,depth) VALUES(?,?,?,?)`, id, b.RootSessionID, b.ParentTaskID, b.Depth); err != nil {
		t.Fatal(err)
	}
	return task
}

func stageWaitTarget(t *testing.T, f *backgroundPermissionFixture, child *bt.Task) interaction.TaskWaitBinding {
	t.Helper()
	wait := interaction.TaskWaitBinding{NativeInterruptID: "native/wait/" + child.Spec.ID, Wait: interaction.TaskWait{TaskID: child.Spec.ID, AfterVersion: child.Version, CallID: "wait/" + child.Spec.ID, SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, RootBudgetID: f.binding.RootBudgetID}}
	if err := f.attempt.StageTaskWaits(context.Background(), []interaction.TaskWaitBinding{wait}); err != nil {
		t.Fatal(err)
	}
	return wait
}

func finishWaitTarget(t *testing.T, f *backgroundPermissionFixture, child *bt.Task) {
	t.Helper()
	ctx := context.Background()
	running, err := f.native.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: child.Spec.ID, ExpectedVersion: child.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.Tasks().Complete(ctx, &bt.CompleteTaskRequest{TaskID: child.Spec.ID, ExpectedVersion: running.Version}); err != nil {
		t.Fatal(err)
	}
}

func pausedWaitFixture(t *testing.T) (*backgroundPermissionFixture, *bt.Task) {
	t.Helper()
	f := newBackgroundPermissionFixture(t)
	child := addWaitTarget(t, f, "descendant")
	stageWaitTarget(t, f, child)
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	if f.state.Kind != "task_wait" || f.state.Resumable || len(f.state.WaitingInputs) != 0 || len(f.state.WaitingTasks) != 1 || f.state.WaitingTasks[0] != child.Spec.ID {
		t.Fatalf("invalid wait projection: %+v", f.state)
	}
	return f, child
}

func readyWait(t *testing.T, f *backgroundPermissionFixture) background.Wakeup {
	t.Helper()
	ready, err := f.permissions.ReadyWakeups(context.Background())
	if err != nil || len(ready) != 1 || ready[0].TaskID != f.binding.TaskID {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	return ready[0]
}

func commitWait(f *backgroundPermissionFixture, w background.Wakeup) error {
	return withExecutionTransaction(context.Background(), f.store, func(tx *sql.Tx) error {
		return f.permissions.CommitWakeupTx(context.Background(), tx, f.binding, w)
	})
}

func TestBackgroundWaitReadinessAndAtomicResume(t *testing.T) {
	f, child := pausedWaitFixture(t)
	ctx := context.Background()
	if ready, err := f.permissions.ReadyWakeups(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("woke before child completion: %+v %v", ready, err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); err == nil {
		t.Fatal("human approval manufactured a wait resume")
	}
	finishWaitTarget(t, f, child)
	w := readyWait(t, f)
	wrong := w
	wrong.Data = []byte(`{"different-target":{"wait":"forged"}}`)
	if err := commitWait(f, wrong); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("forged native targets: %v", err)
	}
	wrong = w
	wrong.Version++
	if err := commitWait(f, wrong); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale native version: %v", err)
	}
	// The production service commits the manifest and native transition in one
	// provider transaction. Inject failure after the manifest update to prove
	// the native CAS cannot leave a consumed, unusable wakeup.
	f.tasks = f.native.Tasks().WithHooks(sqlite.TaskHooks{Transition: func(ctx context.Context, tx *sql.Tx, before, after *bt.Task) error {
		if before.Status == bt.StatusWaitingInput && after.Status == bt.StatusPending {
			return f.permissions.CommitWakeupTx(ctx, tx, f.binding, w)
		}
		if before.Status == bt.StatusPending && after.Status == bt.StatusRunning {
			if _, err := tx.ExecContext(ctx, `INSERT INTO harness_background_child_leases VALUES(?,?,?)`, f.binding.ChildSessionID, f.binding.TaskID, after.Attempt); err != nil {
				return err
			}
		}
		return f.permissions.TransitionTx(ctx, tx, background.TaskScope{Binding: f.binding, Attempt: after.Attempt}, before, after)
	}})
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_wait_resume BEFORE UPDATE ON eino_background_tasks WHEN NEW.status='pending' BEGIN SELECT RAISE(ABORT,'resume CAS fault'); END`); err != nil {
		t.Fatal(err)
	}
	resume := func() error {
		_, err := f.tasks.Resume(ctx, &bt.ResumeRequest{TaskID: w.TaskID, ExpectedVersion: w.Version, Data: w.Data})
		return err
	}
	if err := resume(); err == nil {
		t.Fatal("native transition fault was hidden")
	}
	if retry := readyWait(t, f); string(retry.Data) != string(w.Data) || retry.Version != w.Version {
		t.Fatalf("fault consumed wakeup: %+v", retry)
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_wait_resume`); err != nil {
		t.Fatal(err)
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if err := resume(); err == nil {
		t.Fatal("wakeup resumed twice")
	}
	f.open(t)
	if f.scope.Attempt != 2 || len(f.attempt.Hooks().Targets) != 0 {
		t.Fatalf("wait changed attempt/permission authority: %+v %+v", f.scope, f.attempt.Hooks())
	}
	if ready, err := f.permissions.ReadyWakeups(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("released wait enumerated again: %+v %v", ready, err)
	}
}

func TestBackgroundWaitRejectsChangedFrontier(t *testing.T) {
	for _, mutation := range []string{"checkpoint", "event", "receipt", "resume_data"} {
		t.Run(mutation, func(t *testing.T) {
			f, child := pausedWaitFixture(t)
			finishWaitTarget(t, f, child)
			w := readyWait(t, f)
			var err error
			switch mutation {
			case "checkpoint":
				err = f.native.Set(context.Background(), f.binding.TaskID+"/checkpoint", []byte("changed"))
			case "event":
				_, err = f.store.Append(context.Background(), harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "text_delta", Text: "unexpected later event"})
			case "receipt":
				receipt := harness.ToolReceipt{RunID: f.binding.TaskID, SessionID: f.binding.ChildSessionID, ToolCallID: "unexpected", ToolName: "write_file", State: harness.ReceiptStarted, Version: 1}
				data, marshalErr := json.Marshal(receipt)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				_, err = f.native.DB().Exec(`INSERT INTO harness_tool_receipts VALUES(?,?,?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.SessionID, receipt.State, receipt.Version, data)
			case "resume_data":
				if err = commitWait(f, w); err != nil {
					t.Fatal(err)
				}
				err = withExecutionTransaction(context.Background(), f.store, func(tx *sql.Tx) error {
					task, err := readBackgroundNativeTask(context.Background(), tx, f.binding.TaskID)
					if err != nil {
						return err
					}
					task.Version++
					task.PendingResume = []byte(`{"forged":true}`)
					_, err = f.permissions.validateReleasedWait(context.Background(), tx, f.binding, task, task.Attempt+1, true)
					return err
				})
				if !errors.Is(err, harness.ErrPermissionDenied) {
					t.Fatalf("tampered resume accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = commitWait(f, w); err == nil {
				t.Fatal("changed frontier accepted for resume")
			}
			ready, err := f.permissions.ReadyWakeups(context.Background())
			if err == nil || len(ready) != 0 {
				t.Fatalf("corrupt wait returned: %+v %v", ready, err)
			}
			state, err := f.permissions.Interaction(context.Background(), f.actor(), f.binding)
			if err != nil || state.BlockedReason == "" || state.Resumable {
				t.Fatalf("corrupt wait not reported: %+v %v", state, err)
			}
		})
	}
}

func TestBackgroundWaitCorruptManifestDoesNotStarveReadyTask(t *testing.T) {
	f, child := pausedWaitFixture(t)
	finishWaitTarget(t, f, child)
	ctx := context.Background()
	// A deliberately corrupt record sorted before the valid parent. The root
	// input is otherwise unused by background specs, preserving SQL constraints.
	bad := addWaitTarget(t, f, "a-corrupt")
	bad, err := f.native.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: bad.Spec.ID, ExpectedVersion: bad.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.Tasks().WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: bad.Spec.ID, ExpectedVersion: bad.Version, Checkpoint: []byte("bad")}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`INSERT INTO harness_background_specs(task_id,child_session_id,input_id,contract,payload,binding) SELECT ?,?,id,'bad','bad','bad' FROM harness_inputs WHERE run_id=?`, bad.Spec.ID, f.binding.RootSession(), f.binding.OriginRunID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`INSERT INTO harness_background_wait_manifests VALUES(?,'waiting','not-json')`, bad.Spec.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := f.permissions.ReadyWakeups(ctx)
	if err == nil || !strings.Contains(err.Error(), bad.Spec.ID) || len(ready) != 1 || ready[0].TaskID != f.binding.TaskID {
		t.Fatalf("bad record starved ready task: %+v %v", ready, err)
	}
}

func TestBackgroundWaitTargetsRemainOwnedAndCertain(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	child := addWaitTarget(t, f, "descendant")
	w := stageWaitTarget(t, f, child)
	for _, mutate := range []func(*interaction.TaskWaitBinding){
		func(w *interaction.TaskWaitBinding) { w.Wait.TaskID = f.binding.TaskID },
		func(w *interaction.TaskWaitBinding) { w.Wait.RunID = "foreign" },
		func(w *interaction.TaskWaitBinding) { w.Wait.RootBudgetID = "foreign" },
		func(w *interaction.TaskWaitBinding) { w.Wait.SessionID = "foreign" },
		func(w *interaction.TaskWaitBinding) { w.NativeInterruptID = "" },
	} {
		wrong := w
		mutate(&wrong)
		if err := f.attempt.StageTaskWaits(context.Background(), []interaction.TaskWaitBinding{wrong}); err == nil {
			t.Fatalf("forged wait accepted: %+v", wrong)
		}
	}
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	finishWaitTarget(t, f, child)
	if _, err := f.native.DB().Exec(`UPDATE harness_background_bindings SET blocked_reason='uncertain effect' WHERE task_id=?`, child.Spec.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := f.permissions.ReadyWakeups(context.Background()); err != nil || len(ready) != 0 {
		t.Fatalf("uncertain child woke parent: %+v %v", ready, err)
	}
	if _, err := f.native.DB().Exec(`UPDATE harness_background_bindings SET blocked_reason='' WHERE task_id=?`, child.Spec.ID); err != nil {
		t.Fatal(err)
	}
	readyWait(t, f)
}

func TestBackgroundWaitUncertainReceiptRequiresReconciliation(t *testing.T) {
	for _, state := range []harness.ReceiptState{harness.ReceiptStarted, harness.ReceiptUncertain} {
		t.Run(string(state), func(t *testing.T) {
			f, child := pausedWaitFixture(t)
			finishWaitTarget(t, f, child)
			ctx := context.Background()
			receipt := harness.ToolReceipt{RunID: child.Spec.ID, SessionID: child.Spec.ID + "/session", ToolCallID: "external-write", ToolName: "host_opencli", State: state, Version: 1}
			// addWaitTarget only registers native ancestry. Add the minimal
			// private session/run rows needed for a real receipt's foreign keys.
			if _, err := f.native.DB().Exec(`INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at) SELECT ?,cwd,title,mode,model,created_at,updated_at FROM harness_sessions WHERE id=?`, receipt.SessionID, f.binding.ChildSessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.DB().Exec(`INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'completed','fixture','fixture')`, receipt.RunID, receipt.SessionID, receipt.RunID+"/input"); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.native.DB().Exec(`INSERT INTO harness_tool_receipts VALUES(?,?,?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.SessionID, receipt.State, receipt.Version, data); err != nil {
				t.Fatal(err)
			}
			if ready, err := f.permissions.ReadyWakeups(ctx); err != nil || len(ready) != 0 {
				t.Fatalf("unreconciled child woke parent: %+v %v", ready, err)
			}
			receipt.State, receipt.Version = harness.ReceiptCompleted, 2
			data, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.native.DB().Exec(`UPDATE harness_tool_receipts SET state=?,version=?,receipt=? WHERE run_id=? AND tool_call_id=?`, receipt.State, receipt.Version, data, receipt.RunID, receipt.ToolCallID); err != nil {
				t.Fatal(err)
			}
			readyWait(t, f)
		})
	}
}
