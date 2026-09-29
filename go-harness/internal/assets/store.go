// Package assets stores immutable, session-scoped attachment snapshots.
package assets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type Store struct {
	db        *sql.DB
	root      *os.Root
	path      string
	lock      *flock.Flock
	lifecycle sync.RWMutex
	closed    bool
	closing   atomic.Bool
	mu        sync.Mutex
	staged    map[string]*Prepared
}

type record struct {
	Ref       harness.AssetRef
	Path      string
	Workspace string
}

// Prepared keeps snapshots pinned until the accepting transaction completes.
// Call Finish exactly once, including after an Attach or transaction failure.
type Prepared struct {
	Input     []harness.Content
	store     *Store
	session   harness.Session
	records   []record
	refs      []harness.AssetRef
	once      sync.Once
	finishErr error
}

func NewStore(ctx context.Context, path string, db *sql.DB) (*Store, error) {
	if db == nil || !filepath.IsAbs(path) {
		return nil, invalid("asset directory must be absolute and database configured")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, invalid("asset directory must be a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, invalid("asset directory changed while opening")
	}
	lock := flock.New(filepath.Join(path, ".store.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		root.Close()
		lock.Close()
		if err != nil {
			return nil, err
		}
		return nil, harness.ErrBusy
	}
	s := &Store{db: db, root: root, path: path, lock: lock, staged: make(map[string]*Prepared)}
	_, err = db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_assets (id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id),workspace TEXT NOT NULL,path TEXT UNIQUE NOT NULL,metadata BLOB NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS harness_assets_session ON harness_assets(session_id);
CREATE TABLE IF NOT EXISTS harness_input_assets (input_id TEXT NOT NULL REFERENCES harness_inputs(id),asset_id TEXT NOT NULL REFERENCES harness_assets(id),PRIMARY KEY(input_id,asset_id));
CREATE TABLE IF NOT EXISTS harness_artifacts (session_id TEXT NOT NULL REFERENCES harness_sessions(id),run_id TEXT NOT NULL REFERENCES harness_runs(id),tool_call_id TEXT NOT NULL,asset_id TEXT NOT NULL REFERENCES harness_assets(id),PRIMARY KEY(run_id,tool_call_id,asset_id));
`)
	if err == nil {
		err = s.CleanupOrphans(ctx)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.closing.Store(true)
	s.mu.Lock()
	var pending []*Prepared
	for key, p := range s.staged {
		pending = append(pending, p)
		delete(s.staged, key)
	}
	s.mu.Unlock()
	var cleanupErr error
	for _, p := range pending {
		cleanupErr = errors.Join(cleanupErr, p.Finish(false))
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(cleanupErr, s.root.Close(), s.lock.Close())
}

func (s *Store) begin(x harness.Session) (*Prepared, error) {
	s.lifecycle.RLock()
	if s.closed || s.closing.Load() {
		s.lifecycle.RUnlock()
		return nil, errors.New("asset store is closed")
	}
	if x.ID == "" {
		s.lifecycle.RUnlock()
		return nil, invalid("asset session is required")
	}
	return &Prepared{store: s, session: x}, nil
}

func (p *Prepared) Finish(committed bool) error {
	p.once.Do(func() {
		defer p.store.lifecycle.RUnlock()
		if committed {
			return
		}
		// A failed Commit can be ambiguous; never remove a snapshot referenced
		// by committed database state. Uncertain cleanup is left to startup GC.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, r := range p.records {
			var exists bool
			if err := p.store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_assets WHERE id=?)`, r.Ref.ID).Scan(&exists); err != nil {
				p.finishErr = errors.Join(p.finishErr, err)
				continue
			}
			if !exists {
				if err := p.store.root.Remove(r.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
					p.finishErr = errors.Join(p.finishErr, err)
				}
			}
		}
	})
	return p.finishErr
}

func (p *Prepared) Attach(ctx context.Context, tx *sql.Tx, inputID string) error {
	if err := p.attachRecords(ctx, tx); err != nil {
		return err
	}
	for _, ref := range p.refs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_input_assets(input_id,asset_id) VALUES(?,?)`, inputID, ref.ID); err != nil {
			return err
		}
	}
	return nil
}

func (p *Prepared) attachRecords(ctx context.Context, tx *sql.Tx) error {
	var cwd string
	if err := tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, p.session.ID).Scan(&cwd); err != nil {
		return err
	}
	if !session.SameWorkspace(cwd, p.session.CWD) {
		return harness.ErrPermissionDenied
	}
	for _, r := range p.records {
		data, err := json.Marshal(r.Ref)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_assets(id,session_id,workspace,path,metadata,created_at) VALUES(?,?,?,?,?,?)`, r.Ref.ID, p.session.ID, cwd, r.Path, data, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

func (p *Prepared) snapshot(ctx context.Context, source io.Reader, name, mime, kind string, limit int64) (harness.AssetRef, error) {
	id := rand.Text()
	temporary, target := ".pending-"+id, id+".blob"
	f, err := p.store.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return harness.AssetRef{}, err
	}
	defer func() { f.Close(); _ = p.store.root.Remove(temporary) }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), &contextReader{ctx: ctx, reader: io.LimitReader(source, limit+1)})
	if err != nil {
		return harness.AssetRef{}, err
	}
	if n > limit {
		return harness.AssetRef{}, invalid("attachment exceeds its size limit")
	}
	if err = f.Sync(); err != nil {
		return harness.AssetRef{}, err
	}
	if err = f.Close(); err != nil {
		return harness.AssetRef{}, err
	}
	if err = p.store.root.Rename(temporary, target); err != nil {
		return harness.AssetRef{}, err
	}
	ref := harness.AssetRef{ID: id, SessionID: p.session.ID, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), Size: n, MimeType: mime, Name: name, Kind: kind}
	p.records = append(p.records, record{Ref: ref, Path: target, Workspace: p.session.CWD})
	p.refs = append(p.refs, ref)
	return ref, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Store) Resolve(ctx context.Context, sessionID string, ref harness.AssetRef) ([]byte, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return nil, errors.New("asset store is closed")
	}
	return s.resolve(ctx, sessionID, ref)
}

func (s *Store) resolve(ctx context.Context, sessionID string, ref harness.AssetRef) ([]byte, error) {
	if sessionID == "" || ref.SessionID != sessionID {
		return nil, harness.ErrPermissionDenied
	}
	var data []byte
	var path, workspace, sessionCWD string
	err := s.db.QueryRowContext(ctx, `SELECT a.metadata,a.path,a.workspace,s.cwd FROM harness_assets a JOIN harness_sessions s ON s.id=a.session_id WHERE a.id=? AND a.session_id=?`, ref.ID, sessionID).Scan(&data, &path, &workspace, &sessionCWD)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var saved harness.AssetRef
	if err = json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	if saved != ref || !session.SameWorkspace(workspace, sessionCWD) || path != ref.ID+".blob" || filepath.Base(path) != path {
		return nil, harness.ErrPermissionDenied
	}
	f, err := openRead(s.root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Size || ref.Size < 0 || ref.Size > 200*1024*1024 {
		return nil, invalid("asset snapshot size changed")
	}
	result, err := io.ReadAll(&contextReader{ctx: ctx, reader: io.LimitReader(f, ref.Size+1)})
	if err != nil {
		return nil, err
	}
	if int64(len(result)) != ref.Size || fmt.Sprintf("%x", sha256.Sum256(result)) != ref.SHA256 {
		return nil, invalid("asset snapshot digest changed")
	}
	return result, nil
}

// LocalURI validates the snapshot before exposing it to an authorized client.
func (s *Store) LocalURI(ctx context.Context, sessionID string, ref harness.AssetRef) (string, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return "", errors.New("asset store is closed")
	}
	if _, err := s.resolve(ctx, sessionID, ref); err != nil {
		return "", err
	}
	p := filepath.ToSlash(filepath.Join(s.path, ref.ID+".blob"))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String(), nil
}

// CleanupOrphans is serialized with preparation and commit. The store's root
// lock excludes other managers; only our flat, generated snapshot names are
// eligible, never arbitrary files or directories under the configured root.
func (s *Store) CleanupOrphans(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return errors.New("asset store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM harness_assets`)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		live[path] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if live[name] || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		id := strings.TrimSuffix(name, ".blob")
		eligible := strings.HasSuffix(name, ".blob") && generatedID(id)
		if strings.HasPrefix(name, ".pending-") {
			eligible = generatedID(strings.TrimPrefix(name, ".pending-"))
		}
		if eligible {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = s.root.Remove(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func generatedID(id string) bool {
	if len(id) < 20 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func invalid(message string) error { return fmt.Errorf("%w: %s", harness.ErrInvalidInput, message) }
