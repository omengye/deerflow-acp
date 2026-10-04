package background

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type waitResumerFixture struct {
	wakeups []Wakeup
	err     error
	commits int
}

func (w *waitResumerFixture) ReadyWakeups(context.Context) ([]Wakeup, error) { return w.wakeups, w.err }
func (w *waitResumerFixture) CommitWakeupTx(_ context.Context, _ *sql.Tx, b Binding, wakeup Wakeup) error {
	if b.TaskID != wakeup.TaskID {
		return harness.ErrPermissionDenied
	}
	w.commits++
	return nil
}

func pauseWaitFixture(t *testing.T, s *Service, id string) *bt.Task {
	t.Helper()
	native, _, state := claimFixture(t, s, id)
	state.mu.Lock()
	state.ready, state.joined = true, true
	state.writes[id+"/checkpoint"] = checkpointWrite{Data: []byte("runner checkpoint")}
	state.mu.Unlock()
	task, err := s.tasks.WaitInput(context.Background(), &bt.WaitInputTaskRequest{TaskID: id, ExpectedVersion: native.Version, Checkpoint: []byte("native checkpoint")})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestBackgroundWaitScanKeepsValidWakeupsWithErrors(t *testing.T) {
	waits := &waitResumerFixture{err: errors.New("another task has a corrupt manifest")}
	s, _ := newFixture(t, func(c *Config) { c.Waits = waits })
	task := submitFixture(t, s, submission("parent", "wait"))
	native := pauseWaitFixture(t, s, task.ID)
	waits.wakeups = []Wakeup{{TaskID: task.ID, Version: native.Version, Data: []byte(`{"target":true}`)}}
	if err := s.wakeWaiting(context.Background()); !errors.Is(err, waits.err) {
		t.Fatalf("missing scan diagnostic: %v", err)
	}
	got, err := s.tasks.Get(context.Background(), task.ID)
	if err != nil || got.Status != bt.StatusPending || waits.commits != 1 {
		t.Fatalf("valid wakeup skipped: %+v %v commits=%d", got, err, waits.commits)
	}
}

func TestBackgroundWaitStopFencesPreviouslySelectedWakeup(t *testing.T) {
	waits := &waitResumerFixture{}
	s, _ := newFixture(t, func(c *Config) { c.Waits = waits })
	task := submitFixture(t, s, submission("parent", "wait"))
	native := pauseWaitFixture(t, s, task.ID)
	waits.wakeups = []Wakeup{{TaskID: task.ID, Version: native.Version, Data: []byte(`{"target":true}`)}}
	// Keep the native record waiting while the durable tree stop commits. This
	// forces resume to hit the transaction fence, rather than a terminal CAS.
	if _, err := s.store.DB().Exec(`CREATE TRIGGER fail_stop BEFORE UPDATE ON eino_background_tasks WHEN NEW.status='canceled' BEGIN SELECT RAISE(ABORT,'stop delivery failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(context.Background(), actor("parent"), task.ID, "user stop"); err == nil {
		t.Fatal("stop fault hidden")
	}
	if err := s.wakeWaiting(context.Background()); err == nil {
		t.Fatal("stopped tree resumed")
	}
	got, err := s.tasks.Get(context.Background(), task.ID)
	if err != nil || got.Status != bt.StatusWaitingInput || waits.commits != 0 {
		t.Fatalf("stopped wait resumed: %+v %v commits=%d", got, err, waits.commits)
	}
	if _, err = s.store.DB().Exec(`DROP TRIGGER fail_stop`); err != nil {
		t.Fatal(err)
	}
	if err = s.applyStops(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.tasks.Get(context.Background(), task.ID)
	if err != nil || got.Status != bt.StatusCanceled {
		t.Fatalf("stop retry: %+v %v", got, err)
	}
}
