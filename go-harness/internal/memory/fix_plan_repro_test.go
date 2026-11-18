package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Stage-0 reproduction promoted to the default regression suite in stage 4.
// The fake provider explicitly labels its content-dependent rejection.
var stage0RejectedEmbedding = errors.Join(ErrEmbeddingInput, errors.New("fixture rejected one input"))

type stage0RejectingEmbedder struct{ rejected string }

func (*stage0RejectingEmbedder) Identity() string { return "stage0-embedding" }
func (e *stage0RejectingEmbedder) Embed(_ context.Context, texts []string, _ bool) ([][]float64, error) {
	vectors := make([][]float64, len(texts))
	for i, value := range texts {
		if value == e.rejected {
			return nil, stage0RejectedEmbedding
		}
		vectors[i] = []float64{1, 0}
	}
	return vectors, nil
}

func TestMemoryFixReproIndexPendingDoesNotStarveHealthyFacts(t *testing.T) {
	ctx := context.Background()
	s, scope := personalStore(t)
	for i := range 17 {
		if _, err := s.Mutate(ctx, scope, change(fmt.Sprintf("seed-%d", i), fmt.Sprintf("profile.field_%d", i), fmt.Sprintf("fixture value %d", i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(ctx, scope, "", 100)
	if err != nil || len(page.Facts) != 17 {
		t.Fatalf("fixture facts: %d %v", len(page.Facts), err)
	}
	s.SetEmbedder(&stage0RejectingEmbedder{rejected: page.Facts[0].Content})
	for range 3 {
		if err = s.IndexPending(ctx); err != nil && !errors.Is(err, stage0RejectedEmbedding) {
			t.Fatalf("unexpected indexing error: %v", err)
		}
	}
	var indexed int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_vectors WHERE model=?", "stage0-embedding").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 16 {
		t.Fatalf("P2-10: one rejected input starved healthy facts across repeated batches; indexed=%d want=16", indexed)
	}
}
