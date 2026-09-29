package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestRevisionedFactsAreScopedAndSurviveReopen(t *testing.T) {
	ctx := context.Background()
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	a1, err := NewScope(SessionScope, workspaceA, "session-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := NewScope(SessionScope, workspaceA, "session-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b1, err := NewScope(SessionScope, workspaceB, "session-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a1.Key() == a2.Key() || a1.Key() == b1.Key() {
		t.Fatal("scope identity crossed session or workspace")
	}
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(ctx, a1, Candidate{Content: "Prefers concise Chinese explanations", Category: "preference", Confidence: .9, Source: Source{ID: "source/1", Kind: "model", RunID: "run-1", InputID: "input-1", EventSequence: 10, PolicyVersion: "v1"}})
	if err != nil || first.Revision != 1 {
		t.Fatalf("create=%+v err=%v", first, err)
	}
	for _, scope := range []Scope{a2, b1} {
		if _, err := store.Get(ctx, scope, first.ID); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("foreign scope read=%v", err)
		}
	}
	second, err := store.Create(ctx, a1, Candidate{Content: "Uses Go", Category: "context", Confidence: .8, Source: Source{ID: "source/2", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.List(ctx, a1, "", 1)
	if err != nil || len(page.Facts) != 1 || page.Next == "" || page.ScopeRevision != 2 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	next, err := store.List(ctx, a1, page.Next, 1)
	if err != nil || len(next.Facts) != 1 || next.Next != "" || next.Facts[0].ID == page.Facts[0].ID {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err = New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Get(ctx, a1, first.ID)
	if err != nil || reopened.Content != first.Content || reopened.Source.EventSequence != 10 || reopened.Source.RunID != "run-1" {
		t.Fatalf("reopen=%+v err=%v", reopened, err)
	}
	updated, err := store.Replace(ctx, a1, first.ID, first.Revision, Candidate{Content: "Prefers concise technical Chinese explanations", Category: "preference", Confidence: .95, Source: Source{ID: "source/3", Kind: "operator"}})
	if err != nil || updated.Revision != 2 || updated.Content == first.Content {
		t.Fatalf("replace=%+v err=%v", updated, err)
	}
	if _, err := store.Replace(ctx, a1, first.ID, first.Revision, Candidate{Content: "stale", Category: "context", Confidence: .8, Source: Source{ID: "source/4", Kind: "operator"}}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale replace=%v", err)
	}
	original, deleted, err := store.InspectRevision(ctx, a1, first.ID, 1)
	if err != nil || deleted || original.Content != first.Content {
		t.Fatalf("original revision=%+v deleted=%v err=%v", original, deleted, err)
	}
	if err := store.Delete(ctx, a1, first.ID, updated.Revision, Source{ID: "source/5", Kind: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, a1, first.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("deleted fact visible=%v", err)
	}
	tombstone, deleted, err := store.InspectRevision(ctx, a1, first.ID, 3)
	if err != nil || !deleted || tombstone.Content != updated.Content {
		t.Fatalf("tombstone=%+v deleted=%v err=%v", tombstone, deleted, err)
	}
	if _, err := store.Get(ctx, a1, second.ID); err != nil {
		t.Fatal(err)
	}
	version, err := store.Revision(ctx, a1)
	if err != nil || version != 4 {
		t.Fatalf("scope version=%d err=%v", version, err)
	}
	if _, err := store.Clear(ctx, a1, version-1, "clear/stale", "operator"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale clear=%v", err)
	}
	count, err := store.Clear(ctx, a1, version, "clear/final", "operator")
	if err != nil || count != 1 {
		t.Fatalf("clear count=%d err=%v", count, err)
	}
	page, err = store.List(ctx, a1, "", 100)
	if err != nil || len(page.Facts) != 0 || page.ScopeRevision != 5 {
		t.Fatalf("cleared page=%+v err=%v", page, err)
	}
}

func TestScopeAndFactInputValidation(t *testing.T) {
	workspace := t.TempDir()
	for _, kind := range []ScopeKind{"", "global", SessionScope, UserScope, WorkspaceScope} {
		if _, err := NewScope(kind, workspace, "", "", ""); err == nil && kind != WorkspaceScope {
			t.Fatalf("accepted incomplete scope %q", kind)
		}
	}
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(SessionScope, workspace, "session", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []Candidate{
		{Content: "", Category: "context", Confidence: .8, Source: Source{ID: "x", Kind: "operator"}},
		{Content: "valid", Category: "context", Confidence: 1.2, Source: Source{ID: "x", Kind: "operator"}},
		{Content: "valid", Category: "context", Confidence: .8, Source: Source{ID: "x", Kind: "other"}},
	} {
		if _, err := store.Create(ctx, scope, candidate); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted invalid candidate=%+v err=%v", candidate, err)
		}
	}
	version, err := store.Revision(ctx, scope)
	if err != nil || version != 0 {
		t.Fatalf("invalid input changed scope version=%d err=%v", version, err)
	}
}
