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

func TestDrainStopsNewAdmissionsWithoutCancellingActiveRun(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.Attach("active", "owner"); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := c.Begin(context.Background(), "active", "owner")
	if err != nil {
		t.Fatal(err)
	}
	activity := c.SetDraining(true)
	if !activity.Draining || activity.ActiveOperations != 1 || activity.Phases["active"] != "running" || ctx.Err() != nil {
		t.Fatalf("drain changed active run: %+v %v", activity, ctx.Err())
	}
	if _, err := c.Attach("new", "other"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("new attach during drain: %v", err)
	}
	release()
	if _, _, err := c.Begin(context.Background(), "active", "owner"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("new run during drain: %v", err)
	}
	activity = c.Activity()
	if activity.ActiveOperations != 0 || activity.Phases["active"] != "attached" {
		t.Fatalf("drained activity: %+v", activity)
	}
	c.SetDraining(false)
	if _, err := c.Attach("new", "other"); err != nil {
		t.Fatal(err)
	}
	_, release, err = c.Begin(context.Background(), "active", "owner")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestCleanupReservationFencesReconnect(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.Attach("busy", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReserveCleanup("busy"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("attached session reserved: %v", err)
	}
	release, err := c.ReserveCleanup("deleted")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = c.ReserveCleanup("deleted"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("duplicate cleanup: %v", err)
	}
	if _, err = c.Attach("deleted", "racing-owner"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("reconnect during cleanup: %v", err)
	}
	activity := c.Activity()
	if activity.Phases["deleted"] != "deleting" || activity.ActiveOperations != 1 {
		t.Fatalf("cleanup activity: %+v", activity)
	}
	release()
	release()
	if _, err = c.Attach("deleted", "racing-owner"); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerCleanupAtomicallyDetachesIdleSession(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.ReserveOwnerCleanup("missing", "owner"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := c.Attach("session", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReserveOwnerCleanup("session", "other"); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatalf("foreign delete: %v", err)
	}
	_, releaseRun, err := c.Begin(context.Background(), "session", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReserveOwnerCleanup("session", "owner"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("running delete: %v", err)
	}
	releaseRun()
	finish, err := c.ReserveOwnerCleanup("session", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Attach("session", "other"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("reconnect during deletion: %v", err)
	}
	finish(true)
	if err = c.Authorize("session", "owner"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("failed cleanup reopened execution: %v", err)
	}
	if err = c.Detach(context.Background(), "session", "owner"); err != nil {
		t.Fatalf("failed cleanup could not be retried: %v", err)
	}
	if _, err = c.Attach("session", "owner"); err != nil {
		t.Fatal(err)
	}
	finish, err = c.ReserveOwnerCleanup("session", "owner")
	if err != nil {
		t.Fatal(err)
	}
	finish(false)
	if err = c.Authorize("session", "owner"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("deleted attachment survived: %v", err)
	}
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

func TestLifecycleLeasePreventsPromptUntilBindingCompletes(t *testing.T) {
	c := NewCoordinator()
	_, release, fresh, err := c.AttachAndBegin(context.Background(), "s", "owner")
	if err != nil || !fresh {
		t.Fatalf("fresh=%t err=%v", fresh, err)
	}
	if _, _, err = c.Begin(context.Background(), "s", "owner"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("prompt entered binding: %v", err)
	}
	release()
	_, release, fresh, err = c.AttachAndBegin(context.Background(), "s", "owner")
	if err != nil || fresh {
		t.Fatalf("reattach fresh=%t err=%v", fresh, err)
	}
	release()
}

func TestResourceCleanupRetainsOwnershipAfterCloseTimeout(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Attach("s", "old")
	_, finishRun, err := c.Begin(context.Background(), "s", "old")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan context.Context, 1)
	cleanupGate := make(chan struct{})
	defer close(cleanupGate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.DetachWithCleanup(ctx, "s", "old", func(cleanup context.Context) error { started <- cleanup; <-cleanupGate; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("close=%v", err)
	}
	select {
	case <-started:
		t.Fatal("resource cleanup preceded run cleanup")
	default:
	}
	finishRun()
	select {
	case cleanup := <-started:
		if cleanup.Err() != nil {
			t.Fatal("resource cleanup inherited expired caller")
		}
	case <-time.After(time.Second):
		t.Fatal("resource cleanup never started")
	}
	if _, err = c.Attach("s", "new"); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatalf("ownership released before resources: %v", err)
	}
}

func TestResourceCloseFailureCanBeRetriedWithoutLosingBinding(t *testing.T) {
	c := NewCoordinator()
	_, _ = c.Attach("s", "old")
	want := errors.New("resource still running")
	if err := c.DetachWithCleanup(context.Background(), "s", "old", func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("close=%v", err)
	}
	if _, err := c.Attach("s", "new"); !errors.Is(err, harness.ErrAttachedElsewhere) {
		t.Fatalf("failed cleanup lost ownership: %v", err)
	}
	if err := c.DetachWithCleanup(context.Background(), "s", "old", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("s", "new"); err != nil {
		t.Fatal(err)
	}
}
