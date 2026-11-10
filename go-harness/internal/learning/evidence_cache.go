package learning

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

type verifiedExtraction struct {
	request  skillmaintenance.Request
	snapshot extractionSnapshot
	result   string
	active   bool
}

type verifiedExperience struct {
	experience harness.SkillExperience
	dependency experienceDependency
	active     bool
}

type evidenceCache struct {
	tx          *sql.Tx
	extractions map[string]verifiedExtraction
	experiences map[string]verifiedExperience
	sourceTurns int
}

func (c *evidenceCache) admitSourceTurns(count int) bool {
	if count < 1 || count > 32 {
		return false
	}
	if c.sourceTurns > 32-count {
		// This is a working-set boundary, not a lifetime evidence limit.
		clear(c.extractions)
		clear(c.experiences)
		c.sourceTurns = 0
	}
	c.sourceTurns += count
	return true
}

type evidenceCacheKey struct{}

// Repeated members often share one extraction. Cache only inside this exact
// SQLite transaction, retaining temporary activity as a denial. A new
// reservation or output transaction must revalidate.
func withEvidenceCache(ctx context.Context, tx *sql.Tx) context.Context {
	if cache, ok := ctx.Value(evidenceCacheKey{}).(*evidenceCache); ok && cache.tx == tx {
		return ctx
	}
	return context.WithValue(ctx, evidenceCacheKey{}, &evidenceCache{tx: tx, extractions: map[string]verifiedExtraction{}, experiences: map[string]verifiedExperience{}})
}

func transactionEvidenceCache(ctx context.Context, tx *sql.Tx) *evidenceCache {
	cache, _ := ctx.Value(evidenceCacheKey{}).(*evidenceCache)
	if cache != nil && cache.tx == tx {
		return cache
	}
	return nil
}
