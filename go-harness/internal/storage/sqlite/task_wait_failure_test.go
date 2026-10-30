package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

func waitingFailureFixture(t *testing.T) (*Store, *bt.Task) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	task, err := s.Tasks().Create(ctx, &bt.CreateTaskRequest{Spec: bt.Spec{ID: "waiting-parent", ExecutorKey: "fixture", SessionID: "parent-session", NotifySession: true}, LeaseExpiryPolicy: bt.LeaseExpiryFail})
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.Tasks().Start(ctx, &bt.StartTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.Tasks().WaitInput(ctx, &bt.WaitInputTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Checkpoint: []byte("unreplayable wait checkpoint")})
	if err != nil {
		t.Fatal(err)
	}
	return s, task
}

func TestFailWaitingTaskTxCommitsNativeFailureAndOutbox(t *testing.T) {
	s, task := waitingFailureFixture(t)
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before, after, err := FailWaitingTaskTx(ctx, tx, task.Spec.ID, task.Version, "dependency ended with unconfirmed results")
	if err != nil || before.Status != bt.StatusWaitingInput || string(before.Checkpoint) != "unreplayable wait checkpoint" || after.Status != bt.StatusFailed || after.Version != before.Version+1 || after.Attempt != before.Attempt || after.DoneAt == nil || len(after.Checkpoint) != 0 || len(after.PendingResume) != 0 {
		t.Fatalf("invalid transition: before=%+v after=%+v err=%v", before, after, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Returned snapshots do not alias persisted payload or one another.
	before.Checkpoint[0] = 'x'
	after.ResultError = "changed by caller"
	saved, err := s.Tasks().Get(ctx, task.Spec.ID)
	if err != nil || saved.ResultError != "dependency ended with unconfirmed results" || saved.Status != bt.StatusFailed {
		t.Fatalf("persisted result=%+v err=%v", saved, err)
	}
	var raw []byte
	if err = s.DB().QueryRow(`SELECT payload FROM eino_task_notifications WHERE task_id=? ORDER BY seq DESC LIMIT 1`, task.Spec.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var notification bt.Notification
	if err = json.Unmarshal(raw, &notification); err != nil || notification.Kind != bt.NotificationFailed || notification.Version != saved.Version || notification.SessionID != "parent-session" {
		t.Fatalf("failed outbox=%+v err=%v", notification, err)
	}
	if _, err = s.Tasks().Resume(ctx, &bt.ResumeRequest{TaskID: task.Spec.ID, ExpectedVersion: saved.Version}); !errors.Is(err, bt.ErrIllegalTransition) {
		t.Fatalf("failed waiter remained resumable: %v", err)
	}
}

func TestFailWaitingTaskTxRejectsActiveStaleAndTerminalStates(t *testing.T) {
	for _, kind := range []string{"version", "lease", "running_payload", "running_column", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			s, task := waitingFailureFixture(t)
			ctx := context.Background()
			expected := task.Version
			switch kind {
			case "version":
				expected--
			case "lease":
				if _, err := s.DB().Exec(`UPDATE eino_background_tasks SET lease_expires_at=? WHERE id=?`, time.Now().Add(time.Hour).UnixNano(), task.Spec.ID); err != nil {
					t.Fatal(err)
				}
			case "running_payload", "terminal":
				task.Status = bt.StatusRunning
				if kind == "terminal" {
					task.Status = bt.StatusCompleted
				}
				raw, _ := json.Marshal(task)
				if _, err := s.DB().Exec(`UPDATE eino_background_tasks SET payload=? WHERE id=?`, raw, task.Spec.ID); err != nil {
					t.Fatal(err)
				}
			case "running_column":
				if _, err := s.DB().Exec(`UPDATE eino_background_tasks SET status='running' WHERE id=?`, task.Spec.ID); err != nil {
					t.Fatal(err)
				}
			}
			var original []byte
			var notifications int
			if err := s.DB().QueryRow(`SELECT payload FROM eino_background_tasks WHERE id=?`, task.Spec.ID).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().QueryRow(`SELECT count(*) FROM eino_task_notifications`).Scan(&notifications); err != nil {
				t.Fatal(err)
			}
			tx, err := s.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = FailWaitingTaskTx(ctx, tx, task.Spec.ID, expected, "dependency failed")
			if err == nil {
				t.Fatal("unsafe transition accepted")
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var stored []byte
			var currentNotifications int
			if err := s.DB().QueryRow(`SELECT payload FROM eino_background_tasks WHERE id=?`, task.Spec.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().QueryRow(`SELECT count(*) FROM eino_task_notifications`).Scan(&currentNotifications); err != nil {
				t.Fatal(err)
			}
			if string(stored) != string(original) || currentNotifications != notifications {
				t.Fatal("rejected transition mutated task or outbox")
			}
		})
	}
}

func TestFailWaitingTaskTxRollsBackWhenOutboxWriteFails(t *testing.T) {
	s, task := waitingFailureFixture(t)
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_wait_failure_notification BEFORE INSERT ON eino_task_notifications BEGIN SELECT RAISE(ABORT,'fixture outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = FailWaitingTaskTx(context.Background(), tx, task.Spec.ID, task.Version, "dependency failed")
	if err == nil {
		t.Fatal("outbox failure hidden")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Tasks().Get(context.Background(), task.Spec.ID)
	if err != nil || saved.Status != bt.StatusWaitingInput || saved.Version != task.Version || string(saved.Checkpoint) != string(task.Checkpoint) {
		t.Fatalf("failed transaction changed waiter: %+v %v", saved, err)
	}
}
