package agents

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestRoleAttemptTransactionFenceAndDescendantIsolation(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, err := NewController(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	for _, in := range []Invocation{
		{RootID: "root", ID: "parent", Depth: 1},
		{RootID: "root", ID: "child", ParentID: "parent", Depth: 2},
		{RootID: "root", ID: "sibling", Depth: 1},
		{RootID: "different-root", ID: "foreign", ParentID: "parent", Depth: 2},
	} {
		if err = c.Register(ctx, in, 1, 4); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"parent", true}, {"child", true}, {"sibling", false}, {"foreign", false}, {"missing", false}} {
		got, err := IsDescendant(ctx, store.DB(), "root", "parent", tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("%s=%v want %v: %v", tc.id, got, tc.want, err)
		}
	}
	a, err := c.BeginAttempt(ctx, "parent", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	check := func() error {
		tx, err := store.DB().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return a.CheckTx(ctx, tx)
	}
	if err = check(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7 * time.Second)
	if err = check(); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("expired role accepted: %v", err)
	}
	newAttempt, err := c.BeginAttempt(ctx, "parent", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = check(); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("superseded role accepted: %v", err)
	}
	if err = newAttempt.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = newAttempt.CheckTx(ctx, tx); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("finished role accepted: %v", err)
	}
}
