package memory

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSearchRebuildsIndexAndExcludesChangedHeads(t *testing.T) {
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
	scope, err := NewScope(SessionScope, t.TempDir(), "s1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewScope(SessionScope, scope.Workspace(), "s2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(ctx, scope, Candidate{Content: "prefers concise Chinese explanations", Category: "preference", Confidence: .9, Source: Source{ID: "search/1", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(ctx, other, Candidate{Content: "prefers concise Chinese explanations", Category: "preference", Confidence: .9, Source: Source{ID: "search/2", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	assertSearch := func(query string, count int) {
		t.Helper()
		facts, err := store.Search(ctx, scope, query, 10)
		if err != nil || len(facts) != count {
			t.Fatalf("search %q = %+v, %v", query, facts, err)
		}
	}
	assertSearch("concise", 1)
	if store.fts {
		var version int64
		if err := db.DB().QueryRowContext(ctx, `SELECT revision FROM memory_fts_state WHERE scope_key=?`, scope.Key()).Scan(&version); err != nil || version != 1 {
			t.Fatalf("index version=%d err=%v", version, err)
		}
	}
	changed, err := store.Replace(ctx, scope, first.ID, 1, Candidate{Content: "prefers detailed Japanese explanations", Category: "preference", Confidence: .9, Source: Source{ID: "search/3", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	assertSearch("concise", 0)
	assertSearch("Japanese", 1)
	if err := store.Delete(ctx, scope, first.ID, changed.Revision, Source{ID: "search/4", Kind: "operator"}); err != nil {
		t.Fatal(err)
	}
	assertSearch("Japanese", 0)
}

func TestSearchLexicalFallbackForCJKAndUnavailableIndex(t *testing.T) {
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
	scope, err := NewScope(SessionScope, t.TempDir(), "s", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(ctx, scope, Candidate{Content: "喜欢简洁的中文回答", Category: "preference", Confidence: .9, Source: Source{ID: "cjk/1", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{false, true} {
		store.fts = !unavailable
		facts, err := store.Search(ctx, scope, "简洁", 10)
		if err != nil || len(facts) != 1 {
			t.Fatalf("unavailable=%v facts=%+v err=%v", unavailable, facts, err)
		}
	}
}
