package agents

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestControllerTimeSettlementExcludesSuspensionAndUnionsActivity(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "sqlite"}[durable], func(t *testing.T) {
			var db *sql.DB
			if durable {
				store, err := sqlite.Open(filepath.Join(t.TempDir(), "agents.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				db = store.DB()
			}
			c, err := NewController(db)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(100, 0)
			c.now = func() time.Time { return now }
			ctx := context.Background()
			if err = c.Register(ctx, invocation("a"), 1, 1); err != nil {
				t.Fatal(err)
			}
			a, err := c.BeginAttempt(ctx, "a", 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.BeginAttempt(ctx, "a", 30*time.Second); !errors.Is(err, ErrAttemptBusy) {
				t.Fatalf("duplicate start=%v", err)
			}
			first, second := a.Activity("queue"), a.Activity("queue")
			now = now.Add(time.Second)
			first()
			first()
			model := a.Activity("model")
			now = now.Add(time.Second)
			second()
			if err = a.Heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
			if a.Remaining() != 28*time.Second {
				t.Fatalf("elapsed attempt time was not subtracted: %v", a.Remaining())
			}
			if elapsed, _ := c.Elapsed(ctx, "a"); elapsed != 8*time.Second {
				t.Fatalf("no ahead reservation: %v", elapsed)
			}
			now = now.Add(time.Second)
			model()
			if err = a.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			clock, err := c.Timing(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if clock.Elapsed != 3*time.Second || clock.Unconfirmed != 0 || clock.Activities["queue"] != 2*time.Second || clock.Activities["model"] != 2*time.Second {
				t.Fatalf("timings=%+v", clock)
			}
			now = now.Add(10 * time.Hour)
			if err = a.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			if durable {
				c, err = NewController(db)
				if err != nil {
					t.Fatal(err)
				}
				c.now = func() time.Time { return now }
			}
			a, err = c.BeginAttempt(ctx, "a", 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if a.Remaining() != 27*time.Second {
				t.Fatalf("charged approval pause or reset role time: %v", a.Remaining())
			}
			now = now.Add(time.Second)
			if err = a.Finish(ctx); err != nil {
				t.Fatal(err)
			}
			if elapsed, _ := c.Elapsed(ctx, "a"); elapsed != 4*time.Second {
				t.Fatalf("elapsed=%v", elapsed)
			}
		})
	}
}

func TestControllerCrashRetainsReservedTimeAndFencesOldAttempt(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(100, 0)
	newController := func() *Controller {
		c, err := NewController(store.DB())
		if err != nil {
			t.Fatal(err)
		}
		c.now = func() time.Time { return now }
		return c
	}
	c := newController()
	ctx := context.Background()
	if err = c.Register(ctx, invocation("a"), 1, 1); err != nil {
		t.Fatal(err)
	}
	old, err := c.BeginAttempt(ctx, "a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	endModel := old.Activity("model")
	now = now.Add(2 * time.Second)
	if err = old.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	c = newController()
	if _, err = c.BeginAttempt(ctx, "a", 30*time.Second); !errors.Is(err, ErrAttemptBusy) {
		t.Fatalf("unexpired lease reused: %v", err)
	}
	now = now.Add(7 * time.Second)
	a, err := c.BeginAttempt(ctx, "a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if a.Remaining() != 22*time.Second {
		t.Fatalf("crash reset time: %v", a.Remaining())
	}
	endModel()
	for _, check := range []func(context.Context) error{old.Check, old.Heartbeat, old.Finish} {
		if err = check(ctx); !errors.Is(err, ErrAttemptConflict) {
			t.Fatalf("old attempt changed successor: %v", err)
		}
	}
	now = now.Add(time.Second)
	if err = a.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	clock, err := c.Timing(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if clock.Elapsed != 9*time.Second || clock.Unconfirmed != 6*time.Second || clock.Activities["model"] != 2*time.Second {
		t.Fatalf("crash timing=%+v", clock)
	}
}

func TestControllerRepeatedCrashCannotResetTimeLimit(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Unix(100, 0)
	c, err := NewController(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if err = c.Register(ctx, invocation("a"), 1, 1); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err = c.BeginAttempt(ctx, "a", 10*time.Second)
		if attempt < 2 && err != nil {
			t.Fatal(err)
		}
		if attempt == 2 && !errors.Is(err, ErrTimeLimit) {
			t.Fatalf("crashes restored time budget: %v", err)
		}
		now = now.Add(7 * time.Second)
		c, err = NewController(store.DB())
		if err != nil {
			t.Fatal(err)
		}
		c.now = func() time.Time { return now }
	}
	if elapsed, err := c.Elapsed(ctx, "a"); err != nil || elapsed != 10*time.Second {
		t.Fatalf("elapsed=%v error=%v", elapsed, err)
	}
}

func TestControllerTimeWriteFailureDoesNotPartiallyReserveOrRefund(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, err := NewController(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Register(ctx, invocation("a"), 1, 1); err != nil {
		t.Fatal(err)
	}
	fail := func() {
		if _, err = store.DB().Exec(`CREATE TRIGGER fail_agent_time BEFORE UPDATE OF elapsed_ns ON harness_agent_invocations BEGIN SELECT RAISE(ABORT,'fixture elapsed write failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	_, err = c.BeginAttempt(ctx, "a", 30*time.Second)
	var persistence interface{ PersistenceFailure() bool }
	if !errors.As(err, &persistence) || !persistence.PersistenceFailure() {
		t.Fatalf("write failure not classified: %v", err)
	}
	if elapsed, _ := c.Elapsed(ctx, "a"); elapsed != 0 {
		t.Fatalf("partial reserve=%v", elapsed)
	}
	if _, err = store.DB().Exec(`DROP TRIGGER fail_agent_time`); err != nil {
		t.Fatal(err)
	}
	a, err := c.BeginAttempt(ctx, "a", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fail()
	if err = a.Finish(ctx); !errors.As(err, &persistence) {
		t.Fatalf("settlement failure not classified: %v", err)
	}
	if elapsed, _ := c.Elapsed(ctx, "a"); elapsed != timeLease {
		t.Fatalf("partial refund=%v", elapsed)
	}
}
