package background

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestJoinedExecutionFailureFailsTaskWithoutQuarantiningResources(t *testing.T) {
	want := errors.New("late provider failure after stream joined")
	joined := false
	s, _ := newFixture(t, func(c *Config) {
		c.Attempts = &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
			return &Attempt{
				JoinAndClose: func(context.Context) error { joined = true; return nil },
				ExecutionFailure: func() error {
					if !joined {
						t.Error("execution failure inspected before cleanup")
					}
					return want
				},
			}, nil
		}}
	})
	task := submitFixture(t, s, submission("parent", "joined-failure"))
	err := s.manager.Execute(context.Background(), task.ID)
	if errors.Is(err, harness.ErrBackgroundUncertain) {
		t.Fatalf("known execution failure quarantined: %v", err)
	}
	got, err := s.Get(context.Background(), actor("parent"), task.ID)
	if err != nil || got.Status != "failed" || got.BlockedReason != "" || !strings.Contains(got.Error, want.Error()) {
		t.Fatalf("failure lost or quarantined: %+v %v", got, err)
	}
	if countRows(t, s.store, "harness_background_child_leases") != 0 {
		t.Fatal("joined failed task retained child lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.DrainAndClose(ctx); err != nil {
		t.Fatalf("ordinary task failure prevented service shutdown: %v", err)
	}
}

func TestReleaseSuspensionVersionChecksNativeTransition(t *testing.T) {
	s, _ := newFixture(t, func(c *Config) {
		c.AdditionalExecutors = []bt.Executor{&fixtureExecutor{run: func(context.Context, *bt.Task, bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
			return &bt.ExecutionResult{Status: bt.StatusSuspended, Checkpoint: []byte("checkpoint")}, nil
		}}}
	})
	ctx := context.Background()
	task := submitFixture(t, s, submission("parent", "versioned-release"))
	if err := s.manager.Execute(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := s.Get(ctx, actor("parent"), task.ID)
	if err != nil || task.Status != "suspended" {
		t.Fatalf("suspend: %+v %v", task, err)
	}
	if _, err = s.ReleaseSuspensionVersion(ctx, actor("parent"), task.ID, 0); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("zero version: %v", err)
	}
	if _, err = s.ReleaseSuspensionVersion(ctx, actor("parent"), task.ID, task.Version-1); !errors.Is(err, bt.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if _, err = s.ReleaseSuspensionVersion(ctx, actor("stranger"), task.ID, task.Version); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("wrong parent: %v", err)
	}
	unchanged, err := s.Get(ctx, actor("parent"), task.ID)
	if err != nil || unchanged.Status != "suspended" || unchanged.Version != task.Version {
		t.Fatalf("rejected release mutated task: %+v %v", unchanged, err)
	}
	released, err := s.ReleaseSuspensionVersion(ctx, actor("parent"), task.ID, task.Version)
	if err != nil || released.Status != "pending" {
		t.Fatalf("release: %+v %v", released, err)
	}
	if _, err = s.ReleaseSuspensionVersion(ctx, actor("parent"), task.ID, task.Version); err == nil {
		t.Fatal("duplicate release accepted")
	}
	if err = s.manager.Execute(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.Get(ctx, actor("parent"), task.ID)
	if err != nil || again.Status != "suspended" || again.Attempt != 2 {
		t.Fatalf("second suspension: %+v %v", again, err)
	}
	if _, err = s.ReleaseSuspensionVersion(ctx, actor("parent"), task.ID, task.Version); !errors.Is(err, bt.ErrVersionConflict) {
		t.Fatalf("old version authorized a different checkpoint: %v", err)
	}
}
