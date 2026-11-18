package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const indexMigration = `
CREATE TABLE memory_index_failures(
 fact_id TEXT NOT NULL, revision INTEGER NOT NULL, model TEXT NOT NULL,
 attempts INTEGER NOT NULL, next_at_ns INTEGER NOT NULL, error_code TEXT NOT NULL, terminal INTEGER NOT NULL,
 PRIMARY KEY(fact_id,revision,model));
CREATE TABLE memory_index_backoff(model TEXT PRIMARY KEY, attempts INTEGER NOT NULL, next_at_ns INTEGER NOT NULL);
`

// Embedders mark only input-specific failures with this sentinel. Unclassified
// failures are service outages and never trigger batch splitting.
var ErrEmbeddingInput = errors.New("embedding input rejected")

func (s *Store) SetIndexConfig(cfg harness.MemoryIndexConfig) error {
	var err error
	cfg, err = cfg.WithDefaults()
	if err != nil {
		return err
	}
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	// Lowering the configured cap must also stop persisted retries. Increasing
	// it does not silently revive a previously terminal revision/model.
	if _, err = s.db.Exec("UPDATE memory_index_failures SET terminal=1 WHERE attempts>=?", cfg.MaxAttempts); err != nil {
		return err
	}
	s.indexConfig = cfg
	return nil
}
func (s *Store) SetEmbedder(e Embedder) {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	s.embedder = e
	s.embedGeneration++
}
func (s *Store) embeddingSnapshot() (Embedder, uint64, harness.MemoryIndexConfig) {
	s.embedMu.RLock()
	defer s.embedMu.RUnlock()
	cfg := s.indexConfig
	if cfg.MaxAttempts == 0 {
		cfg = harness.MemoryIndexConfig{MaxAttempts: 3, RetryDelay: time.Minute, MaxRetryDelay: time.Hour}
	}
	return s.embedder, s.embedGeneration, cfg
}
func (s *Store) indexTime() time.Time {
	if s.indexNow != nil {
		return s.indexNow()
	}
	return time.Now()
}

type indexItem struct {
	id       string
	revision int64
	text     string
}

func indexDelay(cfg harness.MemoryIndexConfig, attempt int) time.Duration {
	d := cfg.RetryDelay
	for i := 1; i < attempt && d < cfg.MaxRetryDelay; i++ {
		d = min(d*2, cfg.MaxRetryDelay)
	}
	return d
}

// IndexPending has one process-local owner and at most 15 embedding calls. The
// client holds the exclusive database process lock; no second process may serve
// this store. Service backoff covers later batches as well as the failed batch.
func (s *Store) IndexPending(ctx context.Context) error {
	if !s.indexMu.TryLock() {
		return nil
	}
	defer s.indexMu.Unlock()
	embedder, generation, cfg := s.embeddingSnapshot()
	if embedder == nil {
		return nil
	}
	model := embedder.Identity()
	now := s.indexTime()
	var next int64
	err := s.db.QueryRowContext(ctx, "SELECT next_at_ns FROM memory_index_backoff WHERE model=?", model).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if next > now.UnixNano() {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.head_revision,r.content FROM memory_facts f
 JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision
 LEFT JOIN memory_vectors v ON v.fact_id=f.id AND v.revision=f.head_revision AND v.model=?
 LEFT JOIN memory_index_failures x ON x.fact_id=f.id AND x.revision=f.head_revision AND x.model=?
 WHERE f.deleted=0 AND v.fact_id IS NULL AND (x.fact_id IS NULL OR (x.terminal=0 AND x.attempts<? AND x.next_at_ns<=?)) ORDER BY f.id LIMIT 16`, model, model, cfg.MaxAttempts, now.UnixNano())
	if err != nil {
		return err
	}
	var items []indexItem
	for rows.Next() {
		var x indexItem
		if err = rows.Scan(&x.id, &x.revision, &x.text); err != nil {
			break
		}
		items = append(items, x)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || len(items) == 0 {
		return err
	}
	queue := [][]indexItem{items}
	calls := 0
	var failures error
	for len(queue) > 0 {
		current, currentGeneration, _ := s.embeddingSnapshot()
		if current == nil || currentGeneration != generation || current.Identity() != model {
			return failures
		}
		batch := queue[0]
		queue = queue[1:]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if calls >= 15 {
			if err = s.saveIndexFailure(ctx, batch, model, generation, cfg, "split_deferred", false); err != nil {
				return err
			}
			continue
		}
		texts := make([]string, len(batch))
		for i := range batch {
			texts[i] = batch[i].text
		}
		calls++
		vectors, e := embedder.Embed(ctx, texts, false)
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(e, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			remaining := append([]indexItem{}, batch...)
			for _, pending := range queue {
				remaining = append(remaining, pending...)
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(ctx.Err(), s.saveIndexFailure(cleanup, remaining, model, generation, cfg, "service_unavailable", true))
		}
		if e == nil && len(vectors) != len(batch) {
			e = errors.New("embedding count mismatch")
		}
		if e != nil {
			failures = errors.Join(failures, e)
			if errors.Is(e, ErrEmbeddingInput) {
				if len(batch) > 1 {
					mid := len(batch) / 2
					// Reach leaves before spending the per-tick call budget. A
					// breadth-first split of 16 bad inputs would spend all 15
					// calls on internal nodes and never advance any retry count.
					queue = append([][]indexItem{batch[:mid], batch[mid:]}, queue...)
					continue
				}
				if err = s.saveIndexFailure(ctx, batch, model, generation, cfg, "input_rejected", true); err != nil {
					return err
				}
				continue
			}
			// Defer every unprocessed sub-batch after an outage; no more calls this tick.
			remaining := append([]indexItem{}, batch...)
			for _, pending := range queue {
				remaining = append(remaining, pending...)
			}
			if err = s.saveIndexFailure(ctx, remaining, model, generation, cfg, "service_unavailable", true); err != nil {
				return err
			}
			return failures
		}
		if err = s.saveIndexVectors(ctx, batch, vectors, model, generation, cfg); err != nil {
			return err
		}
	}
	return failures
}

func (s *Store) withIndexGeneration(ctx context.Context, model string, generation uint64, fn func(*sql.Tx) error) error {
	s.embedMu.RLock()
	defer s.embedMu.RUnlock()
	if s.embedder == nil || s.embedGeneration != generation || s.embedder.Identity() != model {
		return nil
	}
	return transact(ctx, s.db, fn)
}
func (s *Store) saveIndexFailure(ctx context.Context, items []indexItem, model string, generation uint64, cfg harness.MemoryIndexConfig, code string, increment bool) error {
	return s.withIndexGeneration(ctx, model, generation, func(tx *sql.Tx) error {
		now := s.indexTime()
		for _, x := range items {
			var previous int
			err := tx.QueryRowContext(ctx, "SELECT attempts FROM memory_index_failures WHERE fact_id=? AND revision=? AND model=?", x.id, x.revision, model).Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			attempts := previous
			if increment {
				attempts++
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_index_failures(fact_id,revision,model,attempts,next_at_ns,error_code,terminal)
 SELECT id,head_revision,?,?,?,?,? FROM memory_facts WHERE id=? AND head_revision=? AND deleted=0
 ON CONFLICT(fact_id,revision,model) DO UPDATE SET attempts=excluded.attempts,next_at_ns=excluded.next_at_ns,error_code=excluded.error_code,terminal=excluded.terminal`, model, attempts, now.Add(indexDelay(cfg, max(1, attempts))).UnixNano(), code, attempts >= cfg.MaxAttempts, x.id, x.revision); err != nil {
				return err
			}
		}
		if code == "service_unavailable" {
			var previous int
			err := tx.QueryRowContext(ctx, "SELECT attempts FROM memory_index_backoff WHERE model=?", model).Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			attempts := min(previous+1, cfg.MaxAttempts)
			_, err = tx.ExecContext(ctx, "INSERT INTO memory_index_backoff(model,attempts,next_at_ns) VALUES(?,?,?) ON CONFLICT(model) DO UPDATE SET attempts=excluded.attempts,next_at_ns=excluded.next_at_ns", model, attempts, now.Add(indexDelay(cfg, attempts)).UnixNano())
			return err
		}
		return nil
	})
}
func (s *Store) saveIndexVectors(ctx context.Context, items []indexItem, vectors [][]float64, model string, generation uint64, cfg harness.MemoryIndexConfig) error {
	var invalid []indexItem
	err := s.withIndexGeneration(ctx, model, generation, func(tx *sql.Tx) error {
		for i, x := range items {
			if !validVector(vectors[i]) {
				invalid = append(invalid, x)
				continue
			}
			raw, _ := json.Marshal(vectors[i])
			if _, err := tx.ExecContext(ctx, `INSERT INTO memory_vectors(fact_id,revision,model,vector) SELECT id,head_revision,?,? FROM memory_facts WHERE id=? AND head_revision=? AND deleted=0 ON CONFLICT DO NOTHING`, model, raw, x.id, x.revision); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM memory_index_failures WHERE fact_id=? AND revision=? AND model=?", x.id, x.revision, model); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM memory_index_backoff WHERE model=?", model); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_vectors WHERE NOT EXISTS(SELECT 1 FROM memory_facts f WHERE f.id=memory_vectors.fact_id AND f.head_revision=memory_vectors.revision AND f.deleted=0)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM memory_index_failures WHERE NOT EXISTS(SELECT 1 FROM memory_facts f WHERE f.id=memory_index_failures.fact_id AND f.head_revision=memory_index_failures.revision AND f.deleted=0)`)
		return err
	})
	if err != nil {
		return err
	}
	if len(invalid) > 0 {
		return s.saveIndexFailure(ctx, invalid, model, generation, cfg, "invalid_vector", true)
	}
	return nil
}

// IndexFailures returns only current heads for the configured model; fixed
// reason codes expose retry/terminal state without returning user content.
func (s *Store) IndexFailures(ctx context.Context, limit int) ([]harness.MemoryIndexFailure, error) {
	if limit < 1 || limit > 1000 {
		return nil, harness.ErrInvalidInput
	}
	embedder, _, _ := s.embeddingSnapshot()
	if embedder == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT x.fact_id,x.revision,x.model,x.attempts,x.next_at_ns,x.error_code,x.terminal FROM memory_index_failures x JOIN memory_facts f ON f.id=x.fact_id AND f.head_revision=x.revision AND f.deleted=0 WHERE x.model=? ORDER BY x.terminal DESC,x.next_at_ns,x.fact_id LIMIT ?`, embedder.Identity(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []harness.MemoryIndexFailure{}
	for rows.Next() {
		var x harness.MemoryIndexFailure
		var ns int64
		if err = rows.Scan(&x.FactID, &x.Revision, &x.Model, &x.Attempts, &ns, &x.ErrorCode, &x.Terminal); err != nil {
			return nil, err
		}
		x.NextAt = time.Unix(0, ns).UTC()
		result = append(result, x)
	}
	return result, rows.Err()
}
