package session

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestTaskAuthorizationFollowsConnectionLifetime(t *testing.T) {
	c := NewCoordinator()
	if err := c.Authorize("session", "old"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal(err)
	}
	_, release, _, err := c.AttachAndBegin(context.Background(), "session", "old")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Authorize("session", "old"); err != nil {
		t.Fatalf("parent tool needs access while busy: %v", err)
	}
	if err := c.Authorize("session", "other"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal(err)
	}
	// Disconnect retires the owner immediately even while cleanup waits for its
	// active prompt. The old identity never regains authority after reconnect.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Disconnect(ctx, "old")
	if err := c.Authorize("session", "old"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal(err)
	}
	release()
	if err := c.Disconnect(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach("session", "new"); err != nil {
		t.Fatal(err)
	}
	if err := c.Authorize("session", "new"); err != nil {
		t.Fatal(err)
	}
	if err := c.Authorize("session", "old"); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatal(err)
	}
}
