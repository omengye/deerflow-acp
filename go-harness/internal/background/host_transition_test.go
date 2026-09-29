package background

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

func TestHostIdleCancellationProjectionRollback(t *testing.T) {
	s, _ := newFixture(t, func(c *Config) {
		c.OnTransitionTx = func(ctx context.Context, tx *sql.Tx, scope TaskScope, before, after *bt.Task) error {
			if before.Status != bt.StatusPending || after.Status != bt.StatusCanceled {
				return errors.New("unexpected idle transition")
			}
			if scope.Attempt != 0 {
				return errors.New("idle cancel forged running attempt")
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO fixture_host_projection VALUES(?,?)", scope.Binding.TaskID, after.Status)
			return err
		}
	})
	ctx := context.Background()
	if _, err := s.store.DB().Exec(`CREATE TABLE fixture_host_projection(task_id TEXT PRIMARY KEY,state TEXT); CREATE TRIGGER fixture_host_fault BEFORE INSERT ON fixture_host_projection BEGIN SELECT RAISE(ABORT,'host projection fault'); END`); err != nil {
		t.Fatal(err)
	}
	task := submitFixture(t, s, submission("parent", "idle"))
	if _, err := s.Cancel(ctx, actor("parent"), task.ID, "stop"); err == nil {
		t.Fatal("expected host projection failure")
	}
	got, err := s.Get(ctx, actor("parent"), task.ID)
	if err != nil || got.Status != "pending" || got.Version != task.Version {
		t.Fatalf("partial cancellation: %+v %v", got, err)
	}
	if countRows(t, s.store, "fixture_host_projection") != 0 {
		t.Fatal("host projection committed on fault")
	}
	if _, err := s.store.DB().Exec("DROP TRIGGER fixture_host_fault"); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Cancel(ctx, actor("parent"), task.ID, "stop"); err != nil || got.Status != "canceled" {
		t.Fatalf("cancel=%+v err=%v", got, err)
	}
	var commits int
	if err := s.store.DB().QueryRow("SELECT commits FROM fixture_budget WHERE task_id=?", task.ID).Scan(&commits); err != nil || commits != 0 {
		t.Fatalf("idle cancel ended nonexistent attempt: %d %v", commits, err)
	}
	if countRows(t, s.store, "fixture_host_projection") != 1 {
		t.Fatal("idle cancellation missed host projection")
	}
}
