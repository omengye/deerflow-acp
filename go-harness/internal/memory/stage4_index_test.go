package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
	"time"
)

type stage4Embedder struct {
	model string
	calls int
	embed func(context.Context, []string) ([][]float64, error)
}

func (e *stage4Embedder) Identity() string { return e.model }
func (e *stage4Embedder) Embed(ctx context.Context, texts []string, _ bool) ([][]float64, error) {
	e.calls++
	return e.embed(ctx, texts)
}
func stage4Vectors(_ context.Context, texts []string) ([][]float64, error) {
	v := make([][]float64, len(texts))
	for i := range v {
		v[i] = []float64{1, 0}
	}
	return v, nil
}
func stage4Seed(t *testing.T, s *Store, scope Scope, n int) {
	t.Helper()
	for i := range n {
		if _, err := s.Mutate(t.Context(), scope, change(fmt.Sprintf("seed-%d", i), fmt.Sprintf("profile.field_%d", i), fmt.Sprintf("value %d", i), 0)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryStage4IndexIsolatesPoisonAndCapsRetries(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 17)
	var bad string
	if err := s.db.QueryRow("SELECT r.content FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision ORDER BY f.id LIMIT 1").Scan(&bad); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.indexNow = func() time.Time { return now }
	e := &stage4Embedder{model: "test", embed: func(ctx context.Context, texts []string) ([][]float64, error) {
		for _, text := range texts {
			if text == bad {
				return nil, ErrEmbeddingInput
			}
		}
		return stage4Vectors(ctx, texts)
	}}
	s.SetEmbedder(e)
	for range 3 {
		_ = s.IndexPending(t.Context())
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM memory_vectors").Scan(&n); err != nil || n != 16 {
		t.Fatalf("healthy=%d %v", n, err)
	}
	for range 3 {
		now = now.Add(24 * time.Hour)
		_ = s.IndexPending(t.Context())
	}
	failures, err := s.IndexFailures(t.Context(), 100)
	if err != nil || len(failures) != 1 || !failures[0].Terminal || failures[0].Attempts != 3 {
		t.Fatalf("%+v %v", failures, err)
	}
	calls := e.calls
	for range 5 {
		now = now.Add(24 * time.Hour)
		_ = s.IndexPending(t.Context())
	}
	if e.calls != calls {
		t.Fatal("terminal poison retried")
	}
	e.model = "new-model"
	s.SetEmbedder(e)
	_ = s.IndexPending(t.Context())
	if e.calls == calls {
		t.Fatal("model change did not reset opportunity")
	}
}

func TestMemoryStage4IndexOutageBackoffSurvivesReopen(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 33)
	now := time.Now()
	s.indexNow = func() time.Time { return now }
	e := &stage4Embedder{model: "outage", embed: func(context.Context, []string) ([][]float64, error) { return nil, errors.New("unavailable") }}
	s.SetEmbedder(e)
	for range 5 {
		_ = s.IndexPending(t.Context())
	}
	if e.calls != 1 {
		t.Fatalf("outage split/storm: %d", e.calls)
	}
	reopened, err := New(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	reopened.indexNow = func() time.Time { return now }
	reopened.SetEmbedder(e)
	_ = reopened.IndexPending(t.Context())
	if e.calls != 1 {
		t.Fatal("restart bypassed backoff")
	}
	now = now.Add(2 * time.Minute)
	_ = reopened.IndexPending(t.Context())
	if e.calls != 2 {
		t.Fatalf("retry calls=%d", e.calls)
	}
}

func TestMemoryStage4IndexLateResultsAndCancellation(t *testing.T) {
	for _, action := range []string{"delete", "correct", "model", "cancel"} {
		t.Run(action, func(t *testing.T) {
			s, scope := personalStore(t)
			stage4Seed(t, s, scope, 1)
			page, err := s.List(t.Context(), scope, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			fact := page.Facts[0]
			e := &stage4Embedder{model: "old"}
			e.embed = func(ctx context.Context, texts []string) ([][]float64, error) {
				switch action {
				case "delete":
					if err := s.Delete(ctx, scope, fact.ID, fact.Revision, Source{ID: "delete", Kind: "operator"}); err != nil {
						t.Fatal(err)
					}
				case "correct":
					if _, err := s.Replace(ctx, scope, fact.ID, fact.Revision, Candidate{Content: "new value", Category: "profile", Confidence: 1, Source: Source{ID: "edit", Kind: "operator"}}); err != nil {
						t.Fatal(err)
					}
				case "model":
					s.SetEmbedder(&stage4Embedder{model: "new", embed: stage4Vectors})
				case "cancel":
					return nil, context.Canceled
				}
				return stage4Vectors(ctx, texts)
			}
			s.SetEmbedder(e)
			_ = s.IndexPending(t.Context())
			var n int
			if err = s.db.QueryRow("SELECT count(*) FROM memory_vectors WHERE model='old' AND revision=1").Scan(&n); err != nil || n != 0 {
				t.Fatalf("late vector: %d %v", n, err)
			}
			if action == "cancel" {
				failures, err := s.IndexFailures(t.Context(), 100)
				if err != nil || len(failures) != 0 {
					t.Fatalf("cancellation consumed retries: %+v %v", failures, err)
				}
			}
		})
	}
}

func TestMemoryStage4IndexConfigAndSplitBound(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 17)
	now := time.Now()
	s.indexNow = func() time.Time { return now }
	if err := s.SetIndexConfig(harness.MemoryIndexConfig{MaxAttempts: -1}); err == nil {
		t.Fatal("invalid config accepted")
	}
	e := &stage4Embedder{model: "all_bad", embed: func(context.Context, []string) ([][]float64, error) { return nil, ErrEmbeddingInput }}
	s.SetEmbedder(e)
	_ = s.IndexPending(t.Context())
	if e.calls > 15 {
		t.Fatalf("unbounded splitting: %d", e.calls)
	}
	for range 20 {
		now = now.Add(24 * time.Hour)
		before := e.calls
		_ = s.IndexPending(t.Context())
		if e.calls-before > 15 {
			t.Fatal("split budget exceeded")
		}
	}
	failures, err := s.IndexFailures(t.Context(), 100)
	if err != nil || len(failures) != 17 {
		t.Fatalf("split made no progress: %+v %v", failures, err)
	}
	for _, f := range failures {
		if !f.Terminal || f.Attempts != 3 {
			t.Fatalf("leaf never resolved: %+v", f)
		}
	}
	before := e.calls
	now = now.Add(24 * time.Hour)
	_ = s.IndexPending(t.Context())
	if e.calls != before {
		t.Fatal("all-bad batch retries forever")
	}
}

func TestMemoryStage4IndexRevisionResetsTerminalFailure(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 1)
	if err := s.SetIndexConfig(harness.MemoryIndexConfig{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	e := &stage4Embedder{model: "test", embed: func(context.Context, []string) ([][]float64, error) { return nil, ErrEmbeddingInput }}
	s.SetEmbedder(e)
	_ = s.IndexPending(t.Context())
	failures, err := s.IndexFailures(t.Context(), 10)
	if err != nil || len(failures) != 1 || !failures[0].Terminal {
		t.Fatalf("%+v %v", failures, err)
	}
	f := failures[0]
	if _, err = s.Replace(t.Context(), scope, f.FactID, f.Revision, Candidate{Content: "corrected", Category: "profile", Confidence: 1, Source: Source{ID: "corrected", Kind: "operator"}}); err != nil {
		t.Fatal(err)
	}
	e.embed = stage4Vectors
	if err = s.IndexPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM memory_vectors WHERE revision=2").Scan(&n); err != nil || n != 1 {
		t.Fatalf("new revision starved: %d %v", n, err)
	}
}

func TestMemoryStage4IndexConcurrentCallsAndMalformedVector(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	e := &stage4Embedder{model: "test", embed: func(ctx context.Context, texts []string) ([][]float64, error) {
		close(entered)
		<-release
		v, _ := stage4Vectors(ctx, texts)
		v[0] = []float64{0, 0}
		return v, nil
	}}
	s.SetEmbedder(e)
	go func() { done <- s.IndexPending(t.Context()) }()
	<-entered
	if err := s.IndexPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Fatal("concurrent index calls")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM memory_vectors").Scan(&n); err != nil || n != 1 {
		t.Fatalf("malformed vector starved healthy sibling: %d %v", n, err)
	}
	failures, err := s.IndexFailures(t.Context(), 10)
	if err != nil || len(failures) != 1 || failures[0].ErrorCode != "invalid_vector" {
		t.Fatalf("%+v %v", failures, err)
	}
}

func TestMemoryStage4IndexTimeoutPersistsBackoff(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 1)
	e := &stage4Embedder{model: "test", embed: func(ctx context.Context, _ []string) ([][]float64, error) { <-ctx.Done(); return nil, ctx.Err() }}
	s.SetEmbedder(e)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := s.IndexPending(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if err := s.IndexPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Fatal("timeout immediately retried")
	}
	failures, err := s.IndexFailures(t.Context(), 10)
	if err != nil || len(failures) != 1 || failures[0].Attempts != 1 {
		t.Fatalf("%+v %v", failures, err)
	}
}

func TestMemoryStage4IndexMigrationAtomicAndRepeatable(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 1)
	if _, err := s.db.Exec("DROP TABLE memory_value_identities; DROP TABLE memory_index_failures; DROP TABLE memory_index_backoff; DELETE FROM memory_schema_migrations WHERE version>=4; CREATE TRIGGER fail_index_migration BEFORE INSERT ON memory_schema_migrations WHEN NEW.version=4 BEGIN SELECT RAISE(ABORT,'index_migration'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), s.db); err == nil {
		t.Fatal("migration fault lost")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('memory_index_failures','memory_index_backoff')").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration: %d %v", n, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_index_migration"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := New(t.Context(), s.db); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRow("SELECT count(*) FROM memory_facts WHERE deleted=0").Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration changed facts: %d %v", n, err)
	}
}

func TestMemoryStage4LoweredIndexCapAppliesToPersistedRetries(t *testing.T) {
	s, scope := personalStore(t)
	stage4Seed(t, s, scope, 1)
	now := time.Now()
	s.indexNow = func() time.Time { return now }
	e := &stage4Embedder{model: "test", embed: func(context.Context, []string) ([][]float64, error) { return nil, ErrEmbeddingInput }}
	s.SetEmbedder(e)
	_ = s.IndexPending(t.Context())
	now = now.Add(time.Hour)
	_ = s.IndexPending(t.Context())
	if err := s.SetIndexConfig(harness.MemoryIndexConfig{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	_ = s.IndexPending(t.Context())
	if e.calls != 2 {
		t.Fatal("lower cap ignored")
	}
	failures, err := s.IndexFailures(t.Context(), 10)
	if err != nil || len(failures) != 1 || !failures[0].Terminal {
		t.Fatalf("%+v %v", failures, err)
	}
	if err = s.SetIndexConfig(harness.MemoryIndexConfig{MaxAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	_ = s.IndexPending(t.Context())
	if e.calls != 2 {
		t.Fatal("increasing cap revived terminal revision")
	}
}
