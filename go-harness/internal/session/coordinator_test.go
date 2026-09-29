package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCancelKeepsBusyUntilCleanup(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.Attach("s", "a"); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := c.Begin(context.Background(), "s", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Cancel("s", "b"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("other connection cancelled this run")
	}
	if err = c.Cancel("s", "a"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("run was not cancelled")
	}
	if _, _, err = c.Begin(context.Background(), "s", "a"); !errors.Is(err, harness.ErrBusy) {
		t.Fatal(err)
	}
	release()
	release()
	_, release, err = c.Begin(context.Background(), "s", "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestDisconnectWaitsForOwnedCleanup(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Attach("a", "one")
	_, _ = c.Attach("b", "two")
	a, finishA, _ := c.Begin(context.Background(), "a", "one")
	b, finishB, _ := c.Begin(context.Background(), "b", "two")
	defer finishB()
	done := make(chan error, 1)
	go func() { done <- c.Disconnect(context.Background(), "one") }()
	select {
	case <-a.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel not delivered")
	}
	select {
	case <-done:
		t.Fatal("disconnect returned before cleanup")
	default:
	}
	if b.Err() != nil {
		t.Fatal("unrelated session cancelled")
	}
	finishA()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("a", "new"); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectedOwnerCannotAttachLate(t *testing.T) {
	c := NewCoordinator()
	if err := c.Disconnect(context.Background(), "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("late", "closed"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("late attach: %v", err)
	}
	if _, err := c.Attach("late", "fresh"); err != nil {
		t.Fatal(err)
	}
}

func TestTimedOutDetachReleasesAfterCleanup(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Attach("s", "old")
	_, release, err := c.Begin(context.Background(), "s", "old")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = c.Detach(ctx, "s", "old"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = c.Attach("s", "new"); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatal(err)
	}
	release()
	if _, err = c.Attach("s", "new"); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectTimeoutStillReleasesIdleSessions(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Attach("busy", "old")
	_, _ = c.Attach("idle", "old")
	_, release, err := c.Begin(context.Background(), "busy", "old")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = c.Disconnect(ctx, "old"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = c.Attach("idle", "new"); err != nil {
		t.Fatalf("idle session leaked: %v", err)
	}
	release()
	if _, err = c.Attach("busy", "new"); err != nil {
		t.Fatalf("busy session leaked: %v", err)
	}
}
