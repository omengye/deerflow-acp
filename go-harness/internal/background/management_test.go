package background

import (
	"context"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestManagementDrainAccountsForBackgroundWork(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var firstID string
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s, _ := newFixture(t, func(c *Config) {
		c.MaxWorkers = 1
		c.AdditionalExecutors = []bt.Executor{&fixtureExecutor{run: func(ctx context.Context, task *bt.Task, _ bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
			if task.Spec.ID == firstID {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &bt.ExecutionResult{Status: bt.StatusCompleted}, nil
		}}}
	})
	firstID = submitFixture(t, s, submission("parent", "one")).ID
	if err := s.StartWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background execution did not start")
	}
	if activity := s.SetDraining(true); !activity.Draining || activity.ActiveOperations != 1 || activity.ActiveRuns != 1 || activity.QueuedRuns != 0 {
		t.Fatalf("running execution disappeared during drain: %+v", activity)
	}
	if _, err := s.Submit(context.Background(), actor("parent"), submission("parent", "two")); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("drain admitted a new task: %v", err)
	}
	close(release)
	waitCondition(t, func() bool { return s.Activity().ActiveOperations == 0 })
	if _, err := s.Submit(context.Background(), actor("parent"), submission("parent", "three")); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("drain admitted a new task after execution: %v", err)
	}
	if activity := s.SetDraining(false); activity.Draining {
		t.Fatalf("resume failed: %+v", activity)
	}
	task := submitFixture(t, s, submission("parent", "two"))
	waitCondition(t, func() bool {
		got, err := s.Get(context.Background(), actor("parent"), task.ID)
		return err == nil && got.Status == "completed"
	})
}
