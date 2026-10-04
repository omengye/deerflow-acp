package agents

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestTaskTreeJournalSeparatesOutcomeAndFencesOldAttempt(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	c, err := NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Register(ctx, invocation("role"), 1, 4); err != nil {
		t.Fatal(err)
	}
	first, err := c.BeginAttempt(ctx, "role", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.RecordState(ctx, "completed", nil); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("active attempt claimed completed: %v", err)
	}
	if err = first.RecordState(ctx, "running", nil); err != nil {
		t.Fatal(err)
	}
	if err = first.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = native.DB().QueryRow(`SELECT state FROM harness_agent_states WHERE invocation_id='role'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("end timing invented outcome: %s %v", state, err)
	}
	if err = first.RecordState(ctx, "waiting_input", nil); err != nil {
		t.Fatal(err)
	}
	second, err := c.BeginAttempt(ctx, "role", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.RecordState(ctx, "running", nil); err != nil {
		t.Fatal(err)
	}
	if err = first.RecordState(ctx, "failed", nil); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("old attempt overwrote successor: %v", err)
	}
	if err = second.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	answer := strings.Repeat("结果", 3000)
	if err = second.RecordState(ctx, "completed", &answer); err != nil {
		t.Fatal(err)
	}
	var result string
	var available, truncated bool
	var version int64
	if err = native.DB().QueryRow(`SELECT state,version,result_text,result_available,result_truncated FROM harness_agent_states WHERE invocation_id='role'`).Scan(&state, &version, &result, &available, &truncated); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || version != 4 || !available || !truncated || len(result) > 8192 || !utf8.ValidString(result) {
		t.Fatalf("bad result state=%s version=%d available=%v truncated=%v bytes=%d", state, version, available, truncated, len(result))
	}
}

func TestTaskTreePersistentStopFencesDescendantsAndLeavesSiblings(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	c, err := NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parent, child, sibling := invocation("parent"), invocation("child"), invocation("sibling")
	child.ParentID, child.Depth = parent.ID, 2
	for _, in := range []Invocation{parent, child, sibling} {
		if err = c.Register(ctx, in, 2, 10); err != nil {
			t.Fatal(err)
		}
	}
	active, err := c.BeginAttempt(ctx, child.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = native.DB().Exec(`INSERT INTO harness_task_tree_stops VALUES('root','parent','now')`); err != nil {
		t.Fatal(err)
	}
	if err = active.CheckStop(ctx); !errors.Is(err, ErrStopped) {
		t.Fatalf("active stop=%v", err)
	}
	tx, err := native.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = active.CheckTx(ctx, tx)
	tx.Rollback()
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("effect stop=%v", err)
	}
	if err = active.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	c, err = NewController(native.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.BeginAttempt(ctx, child.ID, time.Minute); !errors.Is(err, ErrStopped) {
		t.Fatalf("resume escaped stop=%v", err)
	}
	late := invocation("late")
	late.ParentID = child.ID
	late.Depth = 3
	if err = c.Register(ctx, late, 2, 10); !errors.Is(err, ErrStopped) {
		t.Fatalf("new descendant escaped stop=%v", err)
	}
	sib, err := c.BeginAttempt(ctx, sibling.ID, time.Minute)
	if err != nil {
		t.Fatalf("sibling stopped: %v", err)
	}
	if _, err = native.DB().Exec(`INSERT INTO harness_task_tree_stops VALUES('root','','now')`); err != nil {
		t.Fatal(err)
	}
	if err = sib.Check(ctx); !errors.Is(err, ErrStopped) {
		t.Fatalf("root stop=%v", err)
	}
	if err = sib.Finish(ctx); err != nil {
		t.Fatal(err)
	}
}
