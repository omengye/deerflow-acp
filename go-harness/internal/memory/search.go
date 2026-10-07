package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"modernc.org/sqlite"
)

func init() {
	// SQLite lower() folds only ASCII; retain the previous Go Unicode matching.
	sqlite.MustRegisterDeterministicScalarFunction("deerflow_unicode_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		text, _ := args[0].(string)
		return strings.ToLower(text), nil
	})
}

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
	add := func(term string) {
		if term != "" && !seen[term] && len(terms) < 32 {
			terms = append(terms, term)
			seen[term] = true
		}
	}
	// Keep ordinary identifiers whole; segment continuous CJK runs into bigrams.
	// This lets a natural-language question match facts without sending it to a model.
	for _, field := range fields {
		var cjk []rune
		var ordinary strings.Builder
		flush := func() {
			add(ordinary.String())
			ordinary.Reset()
			if len(cjk) == 1 {
				add(string(cjk))
			}
			for i := 1; i < len(cjk); i++ {
				add(string(cjk[i-1 : i+1]))
			}
			cjk = nil
		}
		for _, r := range field {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				if ordinary.Len() > 0 {
					add(ordinary.String())
					ordinary.Reset()
				}
				cjk = append(cjk, r)
			} else {
				if len(cjk) > 0 {
					flush()
				}
				ordinary.WriteRune(r)
			}
		}
		flush()
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
	rows, err := tx.QueryContext(ctx, `SELECT `+factColumns+` FROM memory_facts_fts JOIN memory_facts f ON f.id=memory_facts_fts.fact_id JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE memory_facts_fts.scope_key=? AND memory_facts_fts.content MATCH ? AND f.scope_key=? AND f.deleted=0 ORDER BY bm25(memory_facts_fts),f.id LIMIT ?`, scope.key, ftsQuery(terms), scope.key, limit)
	if err != nil {
		return nil, false, err
	}
	var facts []Fact
	for rows.Next() {
		fact, scanErr := scanFact(rows)
		if scanErr != nil {
			rows.Close()
			return nil, false, scanErr
		}
		facts = append(facts, fact)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	return facts, true, nil
}

func (s *Store) searchLexical(ctx context.Context, scope Scope, terms []string, limit int, includePreferences bool) ([]Fact, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	version, err := checkScope(ctx, tx, scope, false)
	if err != nil {
		return nil, 0, err
	}
	// Rank authoritative current heads in SQLite and transfer only the requested
	// rows. There is no per-fact query or fact-count failure; revision and results
	// still come from this same transaction even if the optional FTS index is stale.
	scores := []string{"0.0"}
	args := []any{}
	if includePreferences {
		scores = append(scores, `CASE WHEN deerflow_unicode_lower(r.category) IN ('preference','profile') THEN 0.5 ELSE 0 END`)
	}
	for _, term := range terms {
		scores = append(scores, `CASE WHEN instr(deerflow_unicode_lower(r.content),?)>0 THEN 1.0 WHEN instr(deerflow_unicode_lower(r.category),?)>0 THEN 0.25 ELSE 0 END`)
		args = append(args, term, term)
	}
	args = append(args, scope.key, limit)
	query := `WITH ranked AS (SELECT f.id,(` + strings.Join(scores, "+") + `) AS score FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0)
SELECT ` + factColumns + ` FROM ranked q JOIN memory_facts f ON f.id=q.id JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE q.score>0 ORDER BY q.score+r.confidence/10 DESC,f.id LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]Fact, 0, limit)
	for rows.Next() {
		fact, err := scanFact(rows)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, fact)
	}
	return result, version, rows.Err()
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
	if s.fts && !containsCJK(query) {
		// Index failure cannot make stored facts unavailable to the agent.
		if err := s.RebuildIndex(ctx, scope); err == nil {
			if facts, current, err := s.searchIndexed(ctx, scope, terms, limit); err == nil && current && len(facts) > 0 {
				return facts, nil
			}
		}
	}
	facts, _, err := s.searchLexical(ctx, scope, terms, limit, false)
	return facts, err
}

func containsCJK(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// SnapshotSearch takes the selected heads and scope revision from one read
// transaction. It is used when a run must pin its injected memory across
// interruption and explicit resume.
func (s *Store) SnapshotSearch(ctx context.Context, scope Scope, query string, limit int) ([]Fact, int64, error) {
	if !utf8.ValidString(query) || len(query) > 1024 || limit < 1 || limit > 100 {
		return nil, 0, harness.ErrInvalidInput
	}
	return s.searchLexical(ctx, scope, searchTerms(query), limit, scope.kind == GlobalScope)
}
