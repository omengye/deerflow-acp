package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ProfileID survives restarts and portable-data copies. It is never derived
// from a connection, model, project path, or an installation's absolute path.
func (s *Store) ProfileID(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_profile(singleton INTEGER PRIMARY KEY CHECK(singleton=1), id TEXT NOT NULL UNIQUE)`); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memory_profile(singleton,id) VALUES(1,?) ON CONFLICT(singleton) DO NOTHING`, rand.Text()); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM memory_profile WHERE singleton=1`).Scan(&id)
	if err == nil {
		_, err = NewScope(GlobalScope, "", "", id, "")
	}
	return id, err
}

// Promote copies the reviewed source head without deleting the original.
// The source version check, destination deduplication, and write are atomic.
func (s *Store) Promote(ctx context.Context, origin, global Scope, id string, expectedRevision int64, source Source) (Fact, error) {
	if origin.kind == GlobalScope || global.kind != GlobalScope || expectedRevision < 1 || source.Kind != "operator" {
		return Fact{}, harness.ErrInvalidInput
	}
	var result Fact
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := checkScope(ctx, tx, origin, false); err != nil {
			return err
		}
		fact, err := readFact(ctx, tx, origin, id)
		if err != nil {
			return err
		}
		if fact.Revision != expectedRevision {
			return ErrVersionConflict
		}
		source.InputID = fact.ID
		source.PolicyVersion = fmt.Sprintf("global-promotion-v1/revision-%d", fact.Revision)
		if !validSource(source) {
			return harness.ErrInvalidInput
		}
		version, err := checkScope(ctx, tx, global, true)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT f.id FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0 AND r.category=? AND lower(trim(r.content))=lower(trim(?)) ORDER BY f.id LIMIT 1`, global.key, fact.Category, fact.Content).Scan(&existing)
		if err == nil {
			result, err = readFact(ctx, tx, global, existing)
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		version, err = bumpScope(ctx, tx, global, version)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = Fact{ID: rand.Text(), ScopeKey: global.key, Revision: 1, Content: fact.Content, Category: fact.Category, Confidence: fact.Confidence, Source: source, CreatedAt: now, UpdatedAt: now}
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,1,?)`, result.ID, result.ScopeKey, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return insertRevision(ctx, tx, result, version, false)
	})
	return result, err
}
