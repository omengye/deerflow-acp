package agents

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func invocation(id string) Invocation {
	return Invocation{RootID: "root", ID: id, Depth: 1, Spec: Spec{Name: "reader", Version: "v1"}}
}

func TestControllerTaskCountAndRecipeSurviveReconstruction(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	ctx := context.Background()
	c, err := NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	first := invocation("first")
	if err = c.Register(ctx, first, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err = c.Charge(ctx, first.ID, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	c, err = NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Register(ctx, first, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err = c.Register(ctx, invocation("second"), 1, 1); !errors.Is(err, ErrTaskLimit) {
		t.Fatalf("reset task limit: %v", err)
	}
	changed := first
	changed.Spec.Version = "v2"
	if err = c.Register(ctx, changed, 1, 1); !errors.Is(err, ErrInvocationConflict) {
		t.Fatalf("mutated saved role: %v", err)
	}
	if elapsed, err := c.Elapsed(ctx, first.ID); err != nil || elapsed != 100*time.Millisecond {
		t.Fatalf("elapsed=%v err=%v", elapsed, err)
	}
}

func TestControllerParallelIOHoldsOneSlotUntilAllJoined(t *testing.T) {
	c, _ := NewController(nil)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := c.Register(ctx, invocation(id), 1, 2); err != nil {
			t.Fatal(err)
		}
	}
	first, err := c.Acquire(ctx, "root", "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Acquire(ctx, "root", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	defer second()
	first()
	blocked, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if release, err := c.Acquire(blocked, "root", "b"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("released before all I/O joined: %v", err)
	}
	second()
	ready, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	release, err := c.Acquire(ready, "root", "b")
	if err != nil {
		t.Fatal(err)
	}
	release()
	// One root's pending work cannot consume another root's slots.
	other := invocation("other")
	other.RootID = "other-root"
	if err := c.Register(ctx, other, 1, 1); err != nil {
		t.Fatal(err)
	}
	release, err = c.Acquire(ctx, other.RootID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestControllerDurableIdleCacheEvictionPreservesTaskLimit(t *testing.T) {
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
	for range 3 {
		first, err := c.Acquire(ctx, "root", "a")
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.Acquire(ctx, "root", "a")
		if err != nil {
			t.Fatal(err)
		}
		first()
		if len(c.roots) != 1 {
			t.Fatal("evicted active I/O")
		}
		second()
		if len(c.roots) != 0 {
			t.Fatal("retained an idle durable root")
		}
		if err = c.Register(ctx, invocation("b"), 1, 1); !errors.Is(err, ErrTaskLimit) {
			t.Fatalf("eviction reset task count: %v", err)
		}
	}
	if _, err = store.DB().Exec(`DELETE FROM harness_agent_invocations; DELETE FROM harness_agent_roots`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Acquire(ctx, "root", "a"); !errors.Is(err, ErrInvocationConflict) {
		t.Fatalf("deleted registration reused: %v", err)
	}
}
