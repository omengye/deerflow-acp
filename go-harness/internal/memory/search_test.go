package memory

import (
	"context"
	"fmt"
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
		facts, current, err := store.searchIndexed(ctx, scope, searchTerms("concise"), 10)
		if err != nil || !current || len(facts) != 1 || facts[0].ID != first.ID {
			t.Fatalf("indexed batch=%v current=%v error=%v", facts, current, err)
		}
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

func TestSearchChineseQuestionAndLargeSnapshot(t *testing.T) {
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
	scope, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(ctx, scope, Candidate{Content: "项目数据库使用 PostgreSQL，迁移脚本使用 Go，维护者 Élodie", Category: "project", Confidence: .9, Source: Source{ID: "database/1", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	// Populate unrelated facts in a single transaction to exercise the former
	// count barrier without spending time on 10,001 independent durable commits.
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 10001; i++ {
		id := fmt.Sprintf("noise-%05d", i)
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts VALUES(?,?,1,0,?)`, id, scope.Key(), target.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_fact_revisions SELECT ?,1,1,'unrelated fact','project',0.1,?,'operator','','','',0,'',0,created_at FROM memory_fact_revisions WHERE fact_id=?`, id, id, target.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"这个项目用什么数据库", "PostgreSQL", "élodie"} {
		facts, revision, err := store.SnapshotSearch(ctx, scope, query, 6)
		if err != nil || len(facts) != 1 || facts[0].ID != target.ID || facts[0].Revision != 1 || revision != 1 {
			t.Fatalf("query=%q facts=%v revision=%d err=%v", query, facts, revision, err)
		}
	}
	if _, err := store.Replace(ctx, scope, target.ID, 1, Candidate{Content: "项目数据库已改为 SQLite", Category: "project", Confidence: .9, Source: Source{ID: "database/2", Kind: "operator"}}); err != nil {
		t.Fatal(err)
	}
	facts, revision, err := store.SnapshotSearch(ctx, scope, "这个项目用什么数据库", 6)
	if err != nil || len(facts) != 1 || facts[0].Revision != 2 || revision != 2 {
		t.Fatalf("changed snapshot=%v revision=%d err=%v", facts, revision, err)
	}
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
