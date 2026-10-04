package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestAutomaticContinuationQuotaRollbackStopAndRecipient(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	enabled, limit := true, 1
	catalog, err := agents.Snapshot(&harness.SubagentsConfig{AutoContinue: &enabled, MaxAutoContinuations: &limit})
	if err != nil {
		t.Fatal(err)
	}
	spec := f.child.spec
	spec.Roles, spec.Automatic = catalog, ContinuationPolicy(catalog)
	b := f.child.binding
	n := bt.Notification{ID: "note", TaskID: b.TaskID, SessionID: b.ParentSessionID, Kind: bt.NotificationCompleted, Version: 3}
	ctx := context.WithValue(context.Background(), automaticNotificationKey{}, true)
	fault := errors.New("commit fault")
	err = executionTx(ctx, f.s, func(tx *sql.Tx) error {
		if err := chargeAutomaticContinuationTx(ctx, tx, b, spec, n); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	var count int
	if err = f.s.db.QueryRow(`SELECT COUNT(*) FROM harness_automatic_continuations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction spent quota %d %v", count, err)
	}
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error { return chargeAutomaticContinuationTx(ctx, tx, b, spec, n) })
	err = executionTx(ctx, f.s, func(tx *sql.Tx) error { return chargeAutomaticContinuationTx(ctx, tx, b, spec, n) })
	if !errors.Is(err, errAutomaticDeferred) {
		t.Fatalf("quota bypass: %v", err)
	}
	// Native recovery and a new owner do not refill this root's allowance.
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error { _, err := f.s.RecoverExecutionsTx(ctx, tx); return err })
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		state, _, err := notificationEligibilityTx(ctx, tx, b, spec, n)
		if err != nil {
			return err
		}
		if state != "limit_reached" {
			t.Fatalf("quota was reset: %s", state)
		}
		b.ParentInvocationID = "private-role"
		state, _, err = notificationEligibilityTx(ctx, tx, b, spec, n)
		if err != nil {
			return err
		}
		if state != "waiting_parent" {
			t.Fatalf("private result retargeted: %s", state)
		}
		b.ParentInvocationID = ""
		row, err := readExecution(ctx, tx, f.req.Session.ID, f.req.RunID)
		if err != nil {
			return err
		}
		if err = stopAutomaticRootTx(ctx, tx, row); err != nil {
			return err
		}
		state, _, err = notificationEligibilityTx(ctx, tx, b, spec, n)
		if err == nil && state != "stopped" {
			t.Fatalf("stop ignored: %s", state)
		}
		return err
	})
}

func TestAutomaticTaskDeliveryCommitsWithExecution(t *testing.T) {
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	lease := f.resume(t, "owner")
	a := &executionAttempt{service: automaticService(t, f, &automaticWaitEngine{}), lease: lease}
	task, err := f.child.tasks.Get(context.Background(), f.child.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	d := interaction.TaskResultDelivery{Wait: f.wait.Wait, TaskVersion: task.Version}
	if err = a.ObserveTaskResult(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.db.QueryRow(`SELECT COUNT(*) FROM harness_task_deliveries`).Scan(&count); err != nil || count != 0 {
		t.Fatal("observation published before commit", err)
	}
	if _, err = f.s.db.Exec(`CREATE TRIGGER delivery_fault BEFORE INSERT ON harness_task_deliveries BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = a.service.finishExecutionAttempt(context.Background(), lease, harness.RunResult{StopReason: "end_turn"}, nil, nil, a.taskResults...)
	if err == nil {
		t.Fatal("injected delivery commit succeeded")
	}
	state, err := f.s.Execution(context.Background(), f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionResuming {
		t.Fatalf("partial execution commit: %+v %v", state, err)
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER delivery_fault`); err != nil {
		t.Fatal(err)
	}
	result, err := a.service.finishExecutionAttempt(context.Background(), lease, harness.RunResult{StopReason: "end_turn"}, nil, nil, a.taskResults...)
	if err != nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("delivery retry: %+v %v", result, err)
	}
	if err = f.s.db.QueryRow(`SELECT COUNT(*) FROM harness_task_deliveries WHERE run_id=?`, f.req.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery count %d %v", count, err)
	}
}
