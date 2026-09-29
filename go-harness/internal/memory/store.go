// Package memory stores scoped, revisioned descriptive facts. Model extraction
// and Eino injection are built on this store; neither model text nor a caller-
// supplied fact ID can choose the authority scope.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type ScopeKind string

const (
	SessionScope   ScopeKind = "session"
	WorkspaceScope ScopeKind = "workspace"
	UserScope      ScopeKind = "user"
)

var ErrVersionConflict = errors.New("memory fact or scope version changed")

// Scope is constructed from host-known attachment identity, not model output.
// User memory remains workspace-bound; global sharing needs an explicit future
// host identity policy and a separate scope kind.
type Scope struct {
	kind      ScopeKind
	workspace string
	subject   string
	agent     string
	key       string
}

func NewScope(kind ScopeKind, workspace, sessionID, userID, agent string) (Scope, error) {
	real, err := session.NormalizeWorkspace(workspace)
	if err != nil {
		return Scope{}, err
	}
	if runtime.GOOS == "windows" {
		real = strings.ToLower(filepath.Clean(real))
	}
	if len(agent) > 128 || !utf8.ValidString(agent) || strings.ContainsRune(agent, 0) {
		return Scope{}, harness.ErrInvalidInput
	}
	var subject string
	switch kind {
	case SessionScope:
		subject = sessionID
	case WorkspaceScope:
		if sessionID != "" || userID != "" {
			return Scope{}, harness.ErrInvalidInput
		}
	case UserScope:
		subject = userID
	default:
		return Scope{}, harness.ErrInvalidInput
	}
	if kind != WorkspaceScope && (subject == "" || len(subject) > 256 || !utf8.ValidString(subject) || strings.ContainsRune(subject, 0)) {
		return Scope{}, harness.ErrInvalidInput
	}
	if kind == SessionScope && userID != "" || kind == UserScope && sessionID != "" {
		return Scope{}, harness.ErrInvalidInput
	}
	raw := "memory/v1\x00" + string(kind) + "\x00" + real + "\x00" + subject + "\x00" + agent
	digest := sha256.Sum256([]byte(raw))
	return Scope{kind: kind, workspace: real, subject: subject, agent: agent, key: hex.EncodeToString(digest[:])}, nil
}

func (s Scope) Key() string       { return s.key }
func (s Scope) Kind() ScopeKind   { return s.kind }
func (s Scope) Workspace() string { return s.workspace }

type Source struct {
	ID            string
	Kind          string // "model" or "operator"
	ActorID       string
	RunID         string
	InputID       string
	EventSequence int64
	PolicyVersion string
}

type Candidate struct {
	Content    string
	Category   string
	Confidence float64
	Source     Source
}

type Fact struct {
	ID         string
	ScopeKey   string
	Revision   int64
	Content    string
	Category   string
	Confidence float64
	Source     Source
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Page struct {
	Facts         []Fact
	ScopeRevision int64
	Next          string
}

type Store struct {
	db  *sql.DB
	fts bool
}

func New(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, harness.ErrInvalidInput
	}
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS memory_scopes (
 key TEXT PRIMARY KEY, kind TEXT NOT NULL, workspace TEXT NOT NULL, subject TEXT NOT NULL, agent TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS memory_facts (
 id TEXT PRIMARY KEY, scope_key TEXT NOT NULL REFERENCES memory_scopes(key), head_revision INTEGER NOT NULL,
 deleted INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS memory_facts_scope ON memory_facts(scope_key,deleted,id);
CREATE TABLE IF NOT EXISTS memory_fact_revisions (
 fact_id TEXT NOT NULL REFERENCES memory_facts(id), revision INTEGER NOT NULL, scope_revision INTEGER NOT NULL,
 content TEXT NOT NULL, category TEXT NOT NULL, confidence REAL NOT NULL,
 source_id TEXT NOT NULL UNIQUE, source_kind TEXT NOT NULL, source_actor_id TEXT NOT NULL, source_run_id TEXT NOT NULL,
 source_input_id TEXT NOT NULL, source_event_sequence INTEGER NOT NULL, policy_version TEXT NOT NULL,
 deleted INTEGER NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(fact_id,revision));`)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_fts_state(scope_key TEXT PRIMARY KEY, revision INTEGER NOT NULL);`); err != nil {
		return nil, err
	}
	if _, err = db.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS memory_facts_fts USING fts5(scope_key UNINDEXED,fact_id UNINDEXED,content,tokenize='unicode61');`); err == nil {
		store.fts = true
	} else if !strings.Contains(err.Error(), "no such module: fts5") {
		return nil, err
	}
	return store, nil
}

func validSource(s Source) bool {
	return s.ID != "" && len(s.ID) <= 256 && utf8.ValidString(s.ID) && !strings.ContainsRune(s.ID, 0) &&
		(s.Kind == "model" || s.Kind == "operator") && s.EventSequence >= 0 &&
		len(s.ActorID) <= 256 && len(s.RunID) <= 256 && len(s.InputID) <= 256 && len(s.PolicyVersion) <= 128 &&
		utf8.ValidString(s.ActorID) && utf8.ValidString(s.RunID) && utf8.ValidString(s.InputID) && utf8.ValidString(s.PolicyVersion)
}

func validCandidate(c Candidate) bool {
	return c.Content != "" && len(c.Content) <= 16*1024 && utf8.ValidString(c.Content) &&
		c.Category != "" && len(c.Category) <= 64 && utf8.ValidString(c.Category) &&
		!math.IsNaN(c.Confidence) && !math.IsInf(c.Confidence, 0) && c.Confidence >= 0 && c.Confidence <= 1 && validSource(c.Source)
}

func transact(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func checkScope(ctx context.Context, tx *sql.Tx, s Scope, create bool) (int64, error) {
	if s.key == "" || s.workspace == "" || s.kind == "" {
		return 0, harness.ErrInvalidInput
	}
	if create {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_scopes(key,kind,workspace,subject,agent) VALUES(?,?,?,?,?) ON CONFLICT(key) DO NOTHING`, s.key, s.kind, s.workspace, s.subject, s.agent); err != nil {
			return 0, err
		}
	}
	var kind, workspace, subject, agent string
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT kind,workspace,subject,agent,revision FROM memory_scopes WHERE key=?`, s.key).Scan(&kind, &workspace, &subject, &agent, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if kind != string(s.kind) || workspace != s.workspace || subject != s.subject || agent != s.agent {
		return 0, harness.ErrInvalidInput
	}
	return version, nil
}

func bumpScope(ctx context.Context, tx *sql.Tx, s Scope, old int64) (int64, error) {
	updated, err := tx.ExecContext(ctx, `UPDATE memory_scopes SET revision=revision+1 WHERE key=? AND revision=?`, s.key, old)
	if err != nil {
		return 0, err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, ErrVersionConflict
	}
	return old + 1, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, f Fact, scopeRevision int64, deleted bool) error {
	flag := 0
	if deleted {
		flag = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_fact_revisions(fact_id,revision,scope_revision,content,category,confidence,source_id,source_kind,source_actor_id,source_run_id,source_input_id,source_event_sequence,policy_version,deleted,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.Revision, scopeRevision, f.Content, f.Category, f.Confidence, f.Source.ID, f.Source.Kind, f.Source.ActorID, f.Source.RunID, f.Source.InputID, f.Source.EventSequence, f.Source.PolicyVersion, flag, f.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) Create(ctx context.Context, scope Scope, candidate Candidate) (Fact, error) {
	if !validCandidate(candidate) {
		return Fact{}, harness.ErrInvalidInput
	}
	now := time.Now().UTC()
	f := Fact{ID: rand.Text(), ScopeKey: scope.key, Revision: 1, Content: candidate.Content, Category: candidate.Category, Confidence: candidate.Confidence, Source: candidate.Source, CreatedAt: now, UpdatedAt: now}
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, true)
		if err != nil {
			return err
		}
		version, err = bumpScope(ctx, tx, scope, version)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,1,?)`, f.ID, f.ScopeKey, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return insertRevision(ctx, tx, f, version, false)
	})
	return f, err
}

func readFact(ctx context.Context, tx *sql.Tx, scope Scope, id string) (Fact, error) {
	var f Fact
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT f.id,f.scope_key,r.revision,r.content,r.category,r.confidence,r.source_id,r.source_kind,r.source_actor_id,r.source_run_id,r.source_input_id,r.source_event_sequence,r.policy_version,f.created_at,r.created_at FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.id=? AND f.scope_key=? AND f.deleted=0`, id, scope.key).Scan(
		&f.ID, &f.ScopeKey, &f.Revision, &f.Content, &f.Category, &f.Confidence, &f.Source.ID, &f.Source.Kind, &f.Source.ActorID, &f.Source.RunID, &f.Source.InputID, &f.Source.EventSequence, &f.Source.PolicyVersion, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, harness.ErrNotFound
	}
	if err != nil {
		return Fact{}, err
	}
	if f.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Fact{}, err
	}
	f.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return f, err
}

func (s *Store) Get(ctx context.Context, scope Scope, id string) (Fact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Fact{}, err
	}
	defer tx.Rollback()
	if _, err = checkScope(ctx, tx, scope, false); err != nil {
		return Fact{}, err
	}
	return readFact(ctx, tx, scope, id)
}

func (s *Store) List(ctx context.Context, scope Scope, after string, limit int) (Page, error) {
	if limit < 0 || limit > 100 || len(after) > 256 {
		return Page{}, harness.ErrInvalidInput
	}
	if limit == 0 {
		limit = 100
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	version, err := checkScope(ctx, tx, scope, false)
	if err != nil {
		return Page{}, err
	}
	page := Page{ScopeRevision: version}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_facts WHERE scope_key=? AND deleted=0 AND id>? ORDER BY id LIMIT ?`, scope.key, after, limit+1)
	if err != nil {
		return Page{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return Page{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Page{}, err
	}
	if len(ids) > limit {
		page.Next = ids[limit-1]
		ids = ids[:limit]
	}
	for _, id := range ids {
		fact, err := readFact(ctx, tx, scope, id)
		if err != nil {
			return Page{}, err
		}
		page.Facts = append(page.Facts, fact)
	}
	return page, nil
}

func (s *Store) Replace(ctx context.Context, scope Scope, id string, expectedRevision int64, candidate Candidate) (Fact, error) {
	if expectedRevision < 1 || !validCandidate(candidate) {
		return Fact{}, harness.ErrInvalidInput
	}
	var f Fact
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, false)
		if err != nil {
			return err
		}
		f, err = readFact(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if f.Revision != expectedRevision {
			return ErrVersionConflict
		}
		version, err = bumpScope(ctx, tx, scope, version)
		if err != nil {
			return err
		}
		f.Revision++
		f.Content, f.Category, f.Confidence, f.Source, f.UpdatedAt = candidate.Content, candidate.Category, candidate.Confidence, candidate.Source, time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE memory_facts SET head_revision=? WHERE id=? AND scope_key=? AND head_revision=? AND deleted=0`, f.Revision, id, scope.key, expectedRevision)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil || changed != 1 {
			return ErrVersionConflict
		}
		return insertRevision(ctx, tx, f, version, false)
	})
	return f, err
}

func (s *Store) Delete(ctx context.Context, scope Scope, id string, expectedRevision int64, source Source) error {
	if expectedRevision < 1 || !validSource(source) {
		return harness.ErrInvalidInput
	}
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, false)
		if err != nil {
			return err
		}
		f, err := readFact(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if f.Revision != expectedRevision {
			return ErrVersionConflict
		}
		version, err = bumpScope(ctx, tx, scope, version)
		if err != nil {
			return err
		}
		f.Revision++
		f.Source, f.UpdatedAt = source, time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE memory_facts SET head_revision=?,deleted=1 WHERE id=? AND scope_key=? AND head_revision=? AND deleted=0`, f.Revision, id, scope.key, expectedRevision)
		if err != nil {
			return err
		}
		changed, err := res.RowsAffected()
		if err != nil || changed != 1 {
			return ErrVersionConflict
		}
		return insertRevision(ctx, tx, f, version, true)
	})
}

func (s *Store) Clear(ctx context.Context, scope Scope, expectedScopeRevision int64, sourcePrefix, actorID string) (int, error) {
	if expectedScopeRevision < 0 || sourcePrefix == "" || len(sourcePrefix) > 128 || !utf8.ValidString(sourcePrefix) || len(actorID) > 256 || !utf8.ValidString(actorID) {
		return 0, harness.ErrInvalidInput
	}
	cleared := 0
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		version, err := checkScope(ctx, tx, scope, false)
		if err != nil {
			return err
		}
		if version != expectedScopeRevision {
			return ErrVersionConflict
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_facts WHERE scope_key=? AND deleted=0 ORDER BY id`, scope.key)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			f, err := readFact(ctx, tx, scope, id)
			if err != nil {
				return err
			}
			version, err = bumpScope(ctx, tx, scope, version)
			if err != nil {
				return err
			}
			f.Revision++
			f.Source = Source{ID: sourcePrefix + "/" + id, Kind: "operator", ActorID: actorID}
			if !validSource(f.Source) {
				return harness.ErrInvalidInput
			}
			f.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE memory_facts SET head_revision=?,deleted=1 WHERE id=? AND scope_key=?`, f.Revision, id, scope.key); err != nil {
				return err
			}
			if err = insertRevision(ctx, tx, f, version, true); err != nil {
				return err
			}
			cleared++
		}
		return nil
	})
	return cleared, err
}

func (s *Store) Revision(ctx context.Context, scope Scope) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	return checkScope(ctx, tx, scope, false)
}

func (s *Store) InspectRevision(ctx context.Context, scope Scope, id string, revision int64) (Fact, bool, error) {
	if revision < 1 {
		return Fact{}, false, harness.ErrInvalidInput
	}
	var f Fact
	var created, updated string
	var deleted bool
	err := s.db.QueryRowContext(ctx, `SELECT f.id,f.scope_key,r.revision,r.content,r.category,r.confidence,r.source_id,r.source_kind,r.source_actor_id,r.source_run_id,r.source_input_id,r.source_event_sequence,r.policy_version,f.created_at,r.created_at,r.deleted FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id WHERE f.id=? AND f.scope_key=? AND r.revision=?`, id, scope.key, revision).Scan(
		&f.ID, &f.ScopeKey, &f.Revision, &f.Content, &f.Category, &f.Confidence, &f.Source.ID, &f.Source.Kind, &f.Source.ActorID, &f.Source.RunID, &f.Source.InputID, &f.Source.EventSequence, &f.Source.PolicyVersion, &created, &updated, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, harness.ErrNotFound
	}
	if err != nil {
		return Fact{}, false, err
	}
	if f.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Fact{}, false, err
	}
	if f.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Fact{}, false, err
	}
	return f, deleted, nil
}

func (s Scope) String() string { return fmt.Sprintf("%s:%s", s.kind, s.key) }
