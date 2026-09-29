package memory

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// RebuildIndex treats FTS as disposable. It snapshots one scope's authoritative
// head revisions and records the exact scope revision represented by the index.
func (s *Store) RebuildIndex(ctx context.Context, scope Scope) error {
	if !s.fts {
		return nil
	}
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, false)
		if err != nil {
			return err
		}
		var indexed int64
		err = tx.QueryRowContext(ctx, `SELECT revision FROM memory_fts_state WHERE scope_key=?`, scope.key).Scan(&indexed)
		if err == nil && indexed == version {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT f.id,r.content FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0 ORDER BY f.id`, scope.key)
		if err != nil {
			return err
		}
		type item struct{ id, content string }
		var facts []item
		for rows.Next() {
			var x item
			if err = rows.Scan(&x.id, &x.content); err != nil {
				rows.Close()
				return err
			}
			facts = append(facts, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_facts_fts WHERE scope_key=?`, scope.key); err != nil {
			return err
		}
		for _, fact := range facts {
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts_fts(scope_key,fact_id,content) VALUES(?,?,?)`, scope.key, fact.id, fact.content); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_fts_state(scope_key,revision) VALUES(?,?) ON CONFLICT(scope_key) DO UPDATE SET revision=excluded.revision`, scope.key, version)
		return err
	})
}

func searchTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r))
	})
	terms := make([]string, 0, min(len(fields), 16))
	seen := make(map[string]bool)
	for _, field := range fields {
		if field != "" && !seen[field] {
			terms = append(terms, field)
			seen[field] = true
		}
		if len(terms) == 16 {
			break
		}
	}
	return terms
}

func ftsQuery(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func (s *Store) searchIndexed(ctx context.Context, scope Scope, terms []string, limit int) ([]Fact, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	version, err := checkScope(ctx, tx, scope, false)
	if err != nil {
		return nil, false, err
	}
	var indexed int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM memory_fts_state WHERE scope_key=?`, scope.key).Scan(&indexed); err != nil || indexed != version {
		return nil, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT fact_id FROM memory_facts_fts WHERE scope_key=? AND content MATCH ? ORDER BY bm25(memory_facts_fts) LIMIT ?`, scope.key, ftsQuery(terms), limit)
	if err != nil {
		return nil, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	var facts []Fact
	for _, id := range ids {
		fact, err := readFact(ctx, tx, scope, id)
		if err != nil {
			return nil, false, err
		}
		facts = append(facts, fact)
	}
	return facts, true, nil
}

func (s *Store) searchLexical(ctx context.Context, scope Scope, terms []string, limit int) ([]Fact, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	version, err := checkScope(ctx, tx, scope, false)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_facts WHERE scope_key=? AND deleted=0 ORDER BY id LIMIT 10001`, scope.key)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if len(ids) > 10000 {
		return nil, 0, harness.ErrInvalidInput
	}
	type scored struct {
		fact  Fact
		score float64
	}
	var matches []scored
	for _, id := range ids {
		fact, err := readFact(ctx, tx, scope, id)
		if err != nil {
			return nil, 0, err
		}
		content := strings.ToLower(fact.Content)
		category := strings.ToLower(fact.Category)
		var hits float64
		for _, term := range terms {
			if strings.Contains(content, term) {
				hits += 1
			} else if strings.Contains(category, term) {
				hits += .25
			}
		}
		if hits > 0 {
			matches = append(matches, scored{fact, hits + fact.Confidence/10})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].fact.ID < matches[j].fact.ID
	})
	result := make([]Fact, 0, min(len(matches), limit))
	for _, match := range matches[:min(len(matches), limit)] {
		result = append(result, match.fact)
	}
	return result, version, nil
}

// Search ranks facts within one host-derived scope. A stale/unavailable FTS5
// index falls back to bounded lexical matching against the authoritative rows.
func (s *Store) Search(ctx context.Context, scope Scope, query string, limit int) ([]Fact, error) {
	if !utf8.ValidString(query) || len(query) > 1024 || limit < 1 || limit > 100 {
		return nil, harness.ErrInvalidInput
	}
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if s.fts {
		// Index failure cannot make stored facts unavailable to the agent.
		if err := s.RebuildIndex(ctx, scope); err == nil {
			if facts, current, err := s.searchIndexed(ctx, scope, terms, limit); err == nil && current && len(facts) > 0 {
				return facts, nil
			}
		}
	}
	facts, _, err := s.searchLexical(ctx, scope, terms, limit)
	return facts, err
}

// SnapshotSearch takes the selected heads and scope revision from one read
// transaction. It is used when a run must pin its injected memory across
// interruption and explicit resume.
func (s *Store) SnapshotSearch(ctx context.Context, scope Scope, query string, limit int) ([]Fact, int64, error) {
	if !utf8.ValidString(query) || len(query) > 1024 || limit < 1 || limit > 100 {
		return nil, 0, harness.ErrInvalidInput
	}
	return s.searchLexical(ctx, scope, searchTerms(query), limit)
}
