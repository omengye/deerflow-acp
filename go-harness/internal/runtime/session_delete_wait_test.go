package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func blockedWaitDeletionFixture(t *testing.T) *executionWaitFixture {
	t.Helper()
	ctx := context.Background()
	f := newExecutionWaitFixture(t)
	f.suspend(t, false)
	f.complete(t)
	lease := f.resume(t, "owner")
	mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
		if _, err := f.s.EndExecutionAttemptTx(ctx, tx, lease, harness.ExecutionStopFailed, "resume failed"); err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, lease.Scope, budget.OutcomeWaitingInput)
	})
	// Model a delivered child notification and an exhausted retry counter
	// for the exact saved checkpoint left by the failed resume.
	if _, err := f.s.db.Exec(`DELETE FROM eino_task_notifications WHERE task_id=?`, f.child.binding.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO harness_execution_wakeups(run_id,checkpoint_sha,attempts,next_at)
 SELECT run_id,json_extract(CAST(manifest AS TEXT),'$.CheckpointSHA'),?,0 FROM harness_executions WHERE run_id=?`, automaticExecutionAttemptLimit, f.req.RunID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExplicitDeleteBlockedWait(t *testing.T) {
	ctx := context.Background()
	f := blockedWaitDeletionFixture(t)
	id := f.req.Session.ID
	if _, err := f.s.db.Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, time.Now().AddDate(0, 0, -31).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	policy := harness.RetentionPolicy{Enabled: true, InactiveDays: 5, ClosedDays: 5, CheckInterval: time.Hour}
	if archived, _, err := f.s.archiveSession(ctx, id, time.Now(), policy); archived || !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("automatic archival of blocked wait: %v %v", archived, err)
	}
	service := NewService(f.s, nil, "")
	if _, err := service.Coordinator.Attach(id, "desktop"); err != nil {
		t.Fatal(err)
	}
	if already, err := service.DeleteAttachedSession(ctx, "desktop", id); err != nil || already {
		t.Fatalf("explicit owner deletion: %v %v", already, err)
	}
	for _, sessionID := range []string{id, f.child.binding.ChildSessionID} {
		if _, err := f.s.Session(ctx, sessionID); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("deleted session %s: %v", sessionID, err)
		}
	}
	for _, table := range []string{"eino_checkpoints", "harness_execution_wakeups", "harness_executions"} {
		var count int
		if err := f.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after purge: %d %v", table, count, err)
		}
	}
	if already, err := service.DeleteSession(ctx, id); err != nil || !already {
		t.Fatalf("deletion retry: %v %v", already, err)
	}
}

func TestExplicitDeleteBlockedWaitRequiresMatchingInactiveAttempt(t *testing.T) {
	for name, change := range map[string]string{
		"retry_available":    `UPDATE harness_execution_wakeups SET attempts=attempts-1`,
		"older_checkpoint":   `UPDATE harness_execution_wakeups SET checkpoint_sha='older'`,
		"mismatched_run":     `UPDATE harness_runs SET status='completed' WHERE status='waiting_input'`,
		"unfinished_retry":   `UPDATE harness_execution_attempts SET finished_at='' WHERE status='failed'`,
		"unknown_outcome":    `UPDATE harness_execution_attempts SET status='future_state' WHERE status='failed'`,
		"wrong_budget_fence": `UPDATE budget_attempts SET fence=fence+1 WHERE id IN (SELECT attempt_id FROM harness_executions WHERE status='waiting_input')`,
		"active_budget":      `UPDATE budget_attempts SET state='active' WHERE id IN (SELECT attempt_id FROM harness_executions WHERE status='waiting_input')`,
		"leased_child":       `UPDATE eino_background_tasks SET lease_expires_at=1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := blockedWaitDeletionFixture(t)
			if _, err := f.s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			service := NewService(f.s, nil, "")
			if _, err := service.DeleteSession(context.Background(), f.req.Session.ID); !errors.Is(err, harness.ErrBusy) {
				t.Fatalf("delete should be refused: %v", err)
			}
			var count int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM eino_checkpoints WHERE id=?`, "harness/turn/v1/"+f.req.RunID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("blocked delete lost checkpoint: %d %v", count, err)
			}
		})
	}
}
