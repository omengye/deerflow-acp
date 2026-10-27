package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func personalStore(t *testing.T) (*Store, Scope) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(context.Background(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.ProfileID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(GlobalScope, "", "", id, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, scope
}
func change(id, key, content string, revision int64) Mutation {
	return Mutation{OperationID: id, Action: "remember", Key: key, Content: content, Category: "profile", ExpectedRevision: revision, Source: Source{ID: id, Kind: "operator", PolicyVersion: "personal-explicit-v1"}}
}

func TestPersonalMemoryCommitReceiptCorrectionAndForget(t *testing.T) {
	ctx := context.Background()
	s, scope := personalStore(t)
	in := change("first", "city", "用户最近告知所在地是北京。", 0)
	saved, err := s.Mutate(ctx, scope, in)
	if err != nil || saved.Status != "saved" {
		t.Fatalf("saved=%+v %v", saved, err)
	}
	replay, err := s.Mutate(ctx, scope, in)
	if err != nil || replay != saved {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	receipt, err := s.Receipt(ctx, scope, in.OperationID)
	if err != nil || receipt != saved {
		t.Fatalf("receipt=%+v %v", receipt, err)
	}
	in.Content = "different"
	if _, err = s.Mutate(ctx, scope, in); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("operation key reused: %v", err)
	}
	updated, err := s.Mutate(ctx, scope, change("next", "profile.last_reported_location", "用户最近告知所在地是上海。", 1))
	if err != nil || updated.Revision != 2 || updated.FactID != saved.FactID {
		t.Fatalf("update=%+v %v", updated, err)
	}
	stale := change("background-old", "city", "用户在北京", 0)
	stale.Automatic = true
	ignored, err := s.Mutate(ctx, scope, stale)
	if err != nil || ignored.Status != "superseded" {
		t.Fatalf("stale=%+v %v", ignored, err)
	}
	forget := change("forget", "city", "", 2)
	forget.Action = "forget"
	deleted, err := s.Mutate(ctx, scope, forget)
	if err != nil || deleted.Status != "forgotten" {
		t.Fatalf("forget=%+v %v", deleted, err)
	}
	if _, err = s.Get(ctx, scope, saved.FactID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("forgotten visible: %v", err)
	}
	stale.OperationID = "resurrect"
	stale.ExpectedRevision = 3
	ignored, err = s.Mutate(ctx, scope, stale)
	if err != nil || ignored.Status != "superseded" {
		t.Fatalf("resurrect=%+v %v", ignored, err)
	}
	if err = s.RebuildIndex(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Search(ctx, scope, "北京 上海", 10); err != nil || len(got) != 0 {
		t.Fatalf("forgotten search=%+v %v", got, err)
	}
	if _, err = s.Mutate(ctx, scope, change("relearn", "city", "用户现在告知所在地是广州。", 3)); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalMemoryIndependentConcurrentAttributesAndCAS(t *testing.T) {
	s, scope := personalStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Mutate(ctx, scope, change(fmt.Sprint(i), fmt.Sprintf("profile.field_%d", i), fmt.Sprint(i), 0)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	page, err := s.List(ctx, scope, "", 100)
	if err != nil || len(page.Facts) != 12 {
		t.Fatalf("page=%+v %v", page, err)
	}
	var successes int
	var mu sync.Mutex
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Mutate(ctx, scope, change(fmt.Sprintf("race%d", i), "profile.field_0", fmt.Sprintf("new%d", i), 1))
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			} else if !errors.Is(err, ErrVersionConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent winners=%d", successes)
	}
}

func TestPersonalProfileAvailableForChineseParaphrases(t *testing.T) {
	s, scope := personalStore(t)
	ctx := context.Background()
	if _, err := s.Mutate(ctx, scope, change("city", "city", "用户最近告知所在地是北京。", 0)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"你知道我在哪吗", "我的所在地是哪里", "帮我查当地天气", "还记得我说的城市吗", ""} {
		facts, _, err := s.SnapshotSearch(ctx, scope, query, 6)
		if err != nil || len(facts) != 1 {
			t.Fatalf("%q facts=%+v err=%v", query, facts, err)
		}
	}
}

type fakeEmbedder struct{ fail bool }

func (*fakeEmbedder) Identity() string { return "fixture-v1" }
func (e *fakeEmbedder) Embed(_ context.Context, texts []string, _ bool) ([][]float64, error) {
	if e.fail {
		return nil, errors.New("offline")
	}
	v := make([][]float64, len(texts))
	for i := range texts {
		v[i] = []float64{1, 0}
	}
	return v, nil
}

func TestPersonalHybridSearchNeverReturnsDeletedOrStaleVectors(t *testing.T) {
	s, _ := personalStore(t)
	ctx := context.Background()
	scope, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	in := change("project", "project.database", "该项目数据库采用 SQLite。", 0)
	in.Category = "project"
	r, err := s.Mutate(ctx, scope, in)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &fakeEmbedder{}
	s.SetEmbedder(embedder)
	if err = s.IndexPending(ctx); err != nil {
		t.Fatal(err)
	}
	facts, _, err := s.HybridSearch(ctx, scope, "数据持久化方式", 5)
	if err != nil || len(facts) != 1 {
		t.Fatalf("semantic=%+v %v", facts, err)
	}
	if err = s.Delete(ctx, scope, r.FactID, 1, Source{ID: "delete", Kind: "operator"}); err != nil {
		t.Fatal(err)
	}
	facts, _, err = s.HybridSearch(ctx, scope, "数据持久化方式", 5)
	if err != nil || len(facts) != 0 {
		t.Fatalf("deleted semantic=%+v %v", facts, err)
	}
	in = change("second", "project.cache", "项目缓存采用 Redis。", 0)
	in.Category = "project"
	if _, err = s.Mutate(ctx, scope, in); err != nil {
		t.Fatal(err)
	}
	embedder.fail = true
	facts, _, err = s.HybridSearch(ctx, scope, "Redis", 5)
	if err != nil || len(facts) != 1 {
		t.Fatalf("offline lexical=%+v %v", facts, err)
	}
}

func TestPersonalExtractionRequiresUserEvidenceAndStableKeys(t *testing.T) {
	valid := `{"facts":[{"scope":"global","key":"city","category":"profile","content":"用户最近告知所在地是北京。","evidence":"我在北京"}]}`
	facts, err := ParseAttributes(valid, "请记住，我在北京")
	if err != nil || facts[0].Key != "profile.last_reported_location" {
		t.Fatalf("facts=%+v %v", facts, err)
	}
	if _, err = ParseAttributes(valid, "你猜我在哪"); err == nil {
		t.Fatal("unsupported model claim accepted")
	}
	if _, err = ParseAttributes(valid+` {}`, "我在北京"); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestPersonalReceiptAndTombstoneSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.ProfileID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := NewScope(GlobalScope, "", "", id, "")
	in := change("forgot-first", "city", "", 0)
	in.Action = "forget"
	r, err := s.Mutate(ctx, scope, in)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err = New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.ProfileID(ctx)
	if err != nil || id2 != id {
		t.Fatalf("profile changed %v", err)
	}
	got, err := s.Receipt(ctx, scope, in.OperationID)
	if err != nil || got != r {
		t.Fatalf("receipt=%+v %v", got, err)
	}
	stale := change("stale", "city", "北京", 0)
	stale.Automatic = true
	got, err = s.Mutate(ctx, scope, stale)
	if err != nil || got.Status != "superseded" {
		t.Fatalf("resurrection=%+v %v", got, err)
	}
}
