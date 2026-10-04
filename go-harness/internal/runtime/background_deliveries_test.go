package runtime

import (
	"context"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func backgroundDeliveryFixture(t *testing.T) (*backgroundPermissionFixture, interaction.TaskResultDelivery) {
	t.Helper()
	f := newBackgroundPermissionFixture(t)
	child := addWaitTarget(t, f, "descendant")
	finishWaitTarget(t, f, child)
	child, err := f.native.Tasks().Get(context.Background(), child.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, interaction.TaskResultDelivery{TaskVersion: child.Version, Wait: interaction.TaskWait{
		TaskID: child.Spec.ID, AfterVersion: child.Version, CallID: "read-result",
		SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, RootBudgetID: f.binding.RootBudgetID,
	}}
}

func assertBackgroundDeliveryCounts(t *testing.T, f *backgroundPermissionFixture, staged, delivered int) {
	t.Helper()
	for table, want := range map[string]int{"harness_background_delivery_stages": staged, "harness_task_deliveries": delivered} {
		var count int
		if err := f.native.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: count=%d want=%d err=%v", table, count, want, err)
		}
	}
}

func TestBackgroundTaskDeliveryAtomicCompletion(t *testing.T) {
	f, d := backgroundDeliveryFixture(t)
	ctx := context.Background()
	for range 2 {
		if err := f.attempt.ObserveTaskResult(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	assertBackgroundDeliveryCounts(t, f, 1, 0)
	changed := d
	changed.Wait.AfterVersion--
	if err := f.attempt.ObserveTaskResult(ctx, changed); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("same call changed its delivered result: %v", err)
	}
	parent, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	complete := func() error {
		_, err := f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: parent.Spec.ID, ExpectedVersion: parent.Version})
		return err
	}
	// The task's native CAS happens after the business transition hook.
	if _, err = f.native.DB().Exec(`CREATE TRIGGER delivery_completion_fault BEFORE UPDATE ON eino_background_tasks WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'native CAS fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err = complete(); err == nil {
		t.Fatal("native completion fault was ignored")
	}
	assertBackgroundDeliveryCounts(t, f, 1, 0)
	if _, err = f.native.DB().Exec(`DROP TRIGGER delivery_completion_fault`); err != nil {
		t.Fatal(err)
	}
	if err = complete(); err != nil {
		t.Fatal(err)
	}
	assertBackgroundDeliveryCounts(t, f, 0, 1)
	var role, run, root, session string
	if err = f.native.DB().QueryRow(`SELECT recipient_role,run_id,root_id,session_id FROM harness_task_deliveries WHERE task_id=?`, d.Wait.TaskID).Scan(&role, &run, &root, &session); err != nil {
		t.Fatal(err)
	}
	if role != "background/"+f.binding.TaskID || run != f.binding.TaskID || root != f.binding.RootBudgetID || session != f.binding.ChildSessionID {
		t.Fatalf("recipient identity lost: %s %s %s %s", role, run, root, session)
	}
	if err = f.attempt.ObserveTaskResult(ctx, d); err == nil {
		t.Fatal("retired attempt published another observation")
	}
}

func TestBackgroundTaskDeliveryCommitsOnlySavedProgress(t *testing.T) {
	for _, outcome := range []string{"permission", "wait", "suspend", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			f, d := backgroundDeliveryFixture(t)
			ctx := context.Background()
			if err := f.attempt.ObserveTaskResult(ctx, d); err != nil {
				t.Fatal(err)
			}
			var err error
			switch outcome {
			case "permission":
				f.pending(t, "new-effect")
				err = f.wait(t)
			case "wait":
				child := addWaitTarget(t, f, "another-descendant")
				stageWaitTarget(t, f, child)
				err = f.wait(t)
			case "suspend":
				_, err = f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Checkpoint: []byte("saved progress")})
			case "failed":
				_, err = f.tasks.Fail(ctx, &bt.FailTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: 2, Error: "model failed before saving progress"})
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if outcome == "failed" {
				want = 0
			}
			assertBackgroundDeliveryCounts(t, f, 0, want)
		})
	}
}

func TestBackgroundTaskDeliveryRejectsInvalidScopeAndVersion(t *testing.T) {
	f, d := backgroundDeliveryFixture(t)
	ctx := context.Background()
	for _, field := range []string{"task", "session", "run", "root", "role", "version", "afterVersion"} {
		t.Run(field, func(t *testing.T) {
			bad := d
			switch field {
			case "task":
				bad.Wait.TaskID = f.binding.TaskID
			case "session":
				bad.Wait.SessionID = f.binding.ParentSessionID
			case "run":
				bad.Wait.RunID = f.binding.OriginRunID
			case "root":
				bad.Wait.RootBudgetID = "another-root"
			case "role":
				bad.Wait.RoleID = "sibling-role"
			case "version":
				bad.TaskVersion++
			case "afterVersion":
				bad.Wait.AfterVersion++
			}
			if err := f.attempt.ObserveTaskResult(ctx, bad); err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
	assertBackgroundDeliveryCounts(t, f, 0, 0)
	if _, err := f.native.DB().Exec(`UPDATE harness_background_bindings SET blocked_reason='uncertain outcome' WHERE task_id=?`, d.Wait.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := f.attempt.ObserveTaskResult(ctx, d); !errors.Is(err, harness.ErrExecutionWaitingInput) {
		t.Fatalf("blocked outcome was consumed: %v", err)
	}
	assertBackgroundDeliveryCounts(t, f, 0, 0)
}
