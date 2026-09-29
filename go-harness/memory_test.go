package deerflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKMemoryScopesOwnerBusyAndReopen(t *testing.T) {
	ctx := context.Background()
	workspace, otherWorkspace := t.TempDir(), t.TempDir()
	cfg := Config{DataDir: t.TempDir(), MemoryUserID: "explicit-user-a", Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	a, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	different, err := c.NewSession(ctx, otherWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	candidate := harness.MemoryCandidate{Content: "Prefers concise explanations", Category: "preference", Confidence: .9}
	sessionFact, err := c.CreateMemoryFact(ctx, a.ID, harness.MemorySession, candidate)
	if err != nil || sessionFact.Revision != 1 || sessionFact.Source.ActorID == "" || sessionFact.Source.Kind != "operator" {
		t.Fatalf("session fact=%+v err=%v", sessionFact, err)
	}
	if err := c.FlushMemory(ctx, a.ID); err != nil {
		t.Fatalf("synchronous memory barrier: %v", err)
	}
	if _, err := c.MemoryFact(ctx, b.ID, harness.MemorySession, sessionFact.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("session fact leaked to peer: %v", err)
	}
	workspaceFact, err := c.CreateMemoryFact(ctx, a.ID, harness.MemoryWorkspace, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MemoryFact(ctx, b.ID, harness.MemoryWorkspace, workspaceFact.ID); err != nil {
		t.Fatalf("same-workspace fact unavailable: %v", err)
	}
	if _, err := c.MemoryFact(ctx, different.ID, harness.MemoryWorkspace, workspaceFact.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("workspace fact leaked: %v", err)
	}
	userFact, err := c.CreateMemoryFact(ctx, a.ID, harness.MemoryUser, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MemoryFact(ctx, b.ID, harness.MemoryUser, userFact.ID); err != nil {
		t.Fatalf("same user/workspace fact unavailable: %v", err)
	}
	if _, err := c.MemoryFact(ctx, different.ID, harness.MemoryUser, userFact.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("user fact leaked across workspace: %v", err)
	}
	_, release, err := c.service.Coordinator.Begin(ctx, a.ID, c.owner)
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 35*time.Millisecond)
	if err := c.FlushMemory(short, a.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush did not wait for active run: %v", err)
	}
	stop()
	if _, err := c.ReplaceMemoryFact(ctx, a.ID, harness.MemorySession, sessionFact.ID, 1, candidate); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("memory changed during active run: %v", err)
	}
	release()
	if err := c.FlushMemory(ctx, a.ID); err != nil {
		t.Fatalf("flush after active run: %v", err)
	}
	changed, err := c.ReplaceMemoryFact(ctx, a.ID, harness.MemorySession, sessionFact.ID, 1, harness.MemoryCandidate{Content: "Prefers short technical answers", Category: "preference", Confidence: .95})
	if err != nil || changed.Revision != 2 {
		t.Fatalf("replace=%+v err=%v", changed, err)
	}
	if err := c.DeleteMemoryFact(ctx, a.ID, harness.MemorySession, sessionFact.ID, 1); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale delete=%v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ session harness.Session }{{a}, {b}, {different}} {
		if _, err := c.LoadSession(ctx, item.session.ID, item.session.CWD, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := c.MemoryFact(ctx, a.ID, harness.MemorySession, sessionFact.ID)
	if err != nil || restored.Content != changed.Content || restored.Revision != changed.Revision {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	page, err := c.MemoryFacts(ctx, a.ID, harness.MemorySession, "", 10)
	if err != nil || page.ScopeRevision != 2 || len(page.Facts) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := c.ClearMemory(ctx, a.ID, harness.MemorySession, page.ScopeRevision-1); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale scope clear=%v", err)
	}
	count, err := c.ClearMemory(ctx, a.ID, harness.MemorySession, page.ScopeRevision)
	if err != nil || count != 1 {
		t.Fatalf("clear count=%d err=%v", count, err)
	}
	if _, err := c.MemoryFact(ctx, a.ID, harness.MemorySession, sessionFact.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("deleted fact visible=%v", err)
	}
	// The scope is based on the resolved directory; a different spelling of
	// the same path cannot silently create a separate memory namespace.
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil || canonical != a.CWD {
		t.Fatalf("workspace identity=%q canonical=%q err=%v", a.CWD, canonical, err)
	}
}
