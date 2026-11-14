package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Periodic housekeeping must not make a concurrent user operation fail with
// ErrBusy, nor absorb a cancel meant for an idle saved wait.
func TestMaintenanceYieldsToUserOperations(t *testing.T) {
	c := NewCoordinator()
	if _, err := c.Attach("s", "a"); err != nil {
		t.Fatal(err)
	}
	hold := func() (context.Context, func()) {
		ctx, release, err := c.BeginMaintenance(context.Background(), "s", "a")
		if err != nil {
			t.Fatal(err)
		}
		return ctx, release
	}
	for name, begin := range map[string]func(context.Context) (context.Context, func(), error){
		"begin": func(ctx context.Context) (context.Context, func(), error) { return c.Begin(ctx, "s", "a") },
		"attach_and_begin": func(ctx context.Context) (context.Context, func(), error) {
			ctx, release, _, err := c.AttachAndBegin(ctx, "s", "a")
			return ctx, release, err
		},
		"cancel_or_begin": func(ctx context.Context) (context.Context, func(), error) { return c.CancelOrBegin(ctx, "s", "a") },
	} {
		maintenanceCtx, releaseMaintenance := hold()
		got := make(chan error, 1)
		go func() {
			_, release, err := begin(context.Background())
			if err == nil && release == nil {
				err = errors.New("reported an active run instead of reserving the idle session")
			}
			if release != nil {
				release()
			}
			got <- err
		}()
		select {
		case err := <-got:
			t.Fatalf("%s did not wait for housekeeping: %v", name, err)
		case <-time.After(50 * time.Millisecond):
		}
		if maintenanceCtx.Err() != nil {
			t.Fatalf("%s cancelled housekeeping", name)
		}
		releaseMaintenance()
		if err := <-got; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Housekeeping itself still cannot overlap real work.
	_, release, err := c.Begin(context.Background(), "s", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.BeginMaintenance(context.Background(), "s", "a"); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("maintenance overlapped a run: %v", err)
	}
	release()
	// A waiting caller can give up.
	_, releaseMaintenance := hold()
	defer releaseMaintenance()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err = c.Begin(ctx, "s", "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting begin ignored its context: %v", err)
	}
}
