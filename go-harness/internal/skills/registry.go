// Package skills manages immutable, explicitly scoped local skill installations.
package skills

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

var ErrClosed = errors.New("skill registry is closed")

type source struct {
	config   harness.SkillSource
	identity string
	root     *os.Root
}
type Registry struct {
	mu      sync.RWMutex
	db      *sql.DB
	sources map[string]*source
	limits  harness.SkillLimits
	closed  bool
}

// NewRegistry owns source directory handles, but never owns or closes db.
// Sources are not implicitly installed; callers explicitly invoke Install.
func NewRegistry(ctx context.Context, config harness.SkillsConfig, db *sql.DB) (*Registry, error) {
	if db == nil {
		return nil, invalid("database is required")
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	if len(config.Sources) > 32 {
		return nil, invalid("too many configured sources")
	}
	r := &Registry{db: db, sources: make(map[string]*source), limits: limits}
	for _, cfg := range config.Sources {
		if !validName(cfg.ID) || r.sources[cfg.ID] != nil {
			_ = r.Close()
			return nil, invalid("source IDs must be unique valid names")
		}
		s, err := openSource(cfg)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		r.sources[cfg.ID] = s
	}
	if err = r.migrate(ctx); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func invalid(s string) error    { return fmt.Errorf("%w: skills %s", harness.ErrInvalidInput, s) }
func denied(s string) error     { return fmt.Errorf("%w: skills %s", harness.ErrPermissionDenied, s) }
func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func normalizeLimits(l harness.SkillLimits) (harness.SkillLimits, error) {
	if l.MaxSkills < 0 || l.MaxFilesPerSkill < 0 || l.MaxVersionsPerSkill < 0 || l.MaxFileBytes < 0 || l.MaxSkillBytes < 0 || l.MaxTotalBytes < 0 {
		return l, invalid("limits cannot be negative")
	}
	if l.MaxSkills == 0 {
		l.MaxSkills = 128
	}
	if l.MaxFilesPerSkill == 0 {
		l.MaxFilesPerSkill = 256
	}
	if l.MaxVersionsPerSkill == 0 {
		l.MaxVersionsPerSkill = 32
	}
	if l.MaxFileBytes == 0 {
		l.MaxFileBytes = 1 << 20
	}
	if l.MaxSkillBytes == 0 {
		l.MaxSkillBytes = 8 << 20
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = 256 << 20
	}
	if l.MaxFilesPerSkill > 10000 || l.MaxSkills > 10000 || l.MaxFileBytes > 64<<20 || l.MaxSkillBytes > 256<<20 {
		return l, invalid("limits exceed supported bounds")
	}
	return l, nil
}

func openSource(cfg harness.SkillSource) (*source, error) {
	if cfg.Scope != harness.SkillScopeGlobal && cfg.Scope != harness.SkillScopeWorkspace {
		return nil, invalid("source scope must be workspace or global")
	}
	if !filepath.IsAbs(cfg.Root) {
		return nil, invalid("source root must be absolute")
	}
	// Reject a symlink in every path component, including the configured root.
	if err := noSymlinkPath(cfg.Root); err != nil {
		return nil, denied("source root contains a link or inaccessible component")
	}
	real, err := session.NormalizeWorkspace(cfg.Root)
	if err != nil {
		return nil, invalid("source root is not an existing directory")
	}
	cfg.Root = real
	if cfg.Scope == harness.SkillScopeWorkspace {
		ws, err := session.NormalizeWorkspace(cfg.Workspace)
		if err != nil {
			return nil, invalid("workspace scope requires an existing absolute workspace")
		}
		rel, err := filepath.Rel(ws, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, denied("workspace source is outside its workspace")
		}
		cfg.Workspace = ws
	} else if cfg.Workspace != "" {
		return nil, invalid("global source cannot specify workspace")
	}
	expected, err := os.Lstat(real)
	if err != nil {
		return nil, invalid("source root is inaccessible")
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return nil, invalid("cannot open source root")
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		_ = root.Close()
		return nil, denied("source root changed during open")
	}
	identityCfg := cfg
	if runtime.GOOS == "windows" {
		identityCfg.Root = strings.ToLower(identityCfg.Root)
		identityCfg.Workspace = strings.ToLower(identityCfg.Workspace)
	}
	b, _ := json.Marshal(identityCfg)
	return &source{config: cfg, identity: digest(b), root: root}, nil
}

func noSymlinkPath(p string) error {
	p = filepath.Clean(p)
	for {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink")
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	for _, s := range r.sources {
		err = errors.Join(err, s.root.Close())
	}
	return err
}

func (r *Registry) migrate(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS df_skill_versions(source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,metadata TEXT NOT NULL,manifest TEXT NOT NULL,total_bytes INTEGER NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(source_key,name,version))`,
		`CREATE TABLE IF NOT EXISTS df_skill_files(source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,path TEXT NOT NULL,hash TEXT NOT NULL,size INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(source_key,name,version,path))`,
		`CREATE TABLE IF NOT EXISTS df_skill_installs(source_key TEXT NOT NULL,name TEXT NOT NULL,current_version TEXT NOT NULL,enabled INTEGER NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(source_key,name))`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("skills schema: %w", err)
		}
	}
	return tx.Commit()
}

// Install copies one source-relative directory into an immutable SQL version.
// New installations are disabled; replacing an installation preserves its flag.
// Old versions remain available for pinned checkpoints until explicit future GC.
func (r *Registry) Install(ctx context.Context, sourceID, relativeDir string) (harness.SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return harness.SkillRecord{}, ErrClosed
	}
	s := r.sources[sourceID]
	if s == nil {
		return harness.SkillRecord{}, denied("source is not configured")
	}
	p, err := r.readPackage(ctx, s, relativeDir)
	if err != nil {
		return harness.SkillRecord{}, err
	}
	metadata, _ := json.Marshal(p.meta)
	manifest, _ := json.Marshal(p.manifest)
	version := digest(manifest)
	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.SkillRecord{}, err
	}
	defer tx.Rollback()
	var enabled int
	var current string
	err = tx.QueryRowContext(ctx, `SELECT current_version,enabled FROM df_skill_installs WHERE source_key=? AND name=?`, s.identity, p.meta.Name).Scan(&current, &enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return harness.SkillRecord{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM df_skill_installs`).Scan(&count); err != nil {
			return harness.SkillRecord{}, err
		}
		if count >= r.limits.MaxSkills {
			return harness.SkillRecord{}, invalid("installed skill count limit exceeded")
		}
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`, s.identity, p.meta.Name, version).Scan(&exists); err != nil {
		return harness.SkillRecord{}, err
	}
	if exists == 0 {
		var count int
		var total int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM df_skill_versions WHERE source_key=? AND name=?`, s.identity, p.meta.Name).Scan(&count); err != nil {
			return harness.SkillRecord{}, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_bytes),0) FROM df_skill_versions`).Scan(&total); err != nil {
			return harness.SkillRecord{}, err
		}
		if count >= r.limits.MaxVersionsPerSkill || p.bytes > r.limits.MaxTotalBytes-total {
			return harness.SkillRecord{}, invalid("retained version or total byte limit exceeded")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_versions(source_key,name,version,metadata,manifest,total_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, s.identity, p.meta.Name, version, string(metadata), string(manifest), p.bytes, now.Format(time.RFC3339Nano)); err != nil {
			return harness.SkillRecord{}, err
		}
		for _, f := range p.manifest {
			if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_files(source_key,name,version,path,hash,size,data) VALUES(?,?,?,?,?,?,?)`, s.identity, p.meta.Name, version, f.Path, f.Hash, f.Bytes, p.files[f.Path]); err != nil {
				return harness.SkillRecord{}, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_installs(source_key,name,current_version,enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET current_version=excluded.current_version,updated_at=excluded.updated_at`, s.identity, p.meta.Name, version, enabled, now.Format(time.RFC3339Nano)); err != nil {
		return harness.SkillRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return harness.SkillRecord{}, err
	}
	return harness.SkillRecord{Ref: harness.SkillRef{SourceID: sourceID, SourceIdentity: s.identity, Name: p.meta.Name, Version: version, Hash: version}, Description: p.meta.Description, Scope: s.config.Scope, Workspace: s.config.Workspace, Enabled: enabled != 0, FileCount: len(p.files), TotalBytes: p.bytes, InstalledAt: now, Findings: p.meta.Findings}, nil
}

func (r *Registry) SetEnabled(ctx context.Context, sourceID, name string, enabled bool) error {
	return r.mutate(ctx, sourceID, name, `UPDATE df_skill_installs SET enabled=?,updated_at=? WHERE source_key=? AND name=?`, enabled, time.Now().UTC().Format(time.RFC3339Nano))
}
func (r *Registry) Delete(ctx context.Context, sourceID, name string) error {
	return r.mutate(ctx, sourceID, name, `DELETE FROM df_skill_installs WHERE source_key=? AND name=?`)
}
func (r *Registry) mutate(ctx context.Context, sourceID, name, q string, args ...any) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	s := r.sources[sourceID]
	if s == nil {
		return denied("source is not configured")
	}
	if !validName(name) {
		return invalid("name is invalid")
	}
	args = append(args, s.identity, name)
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return harness.ErrNotFound
	}
	return err
}

func (r *Registry) selectedSources(selection harness.SkillSelection) ([]*source, error) {
	var workspace string
	if selection.Workspace != "" {
		var err error
		workspace, err = session.NormalizeWorkspace(selection.Workspace)
		if err != nil {
			return nil, invalid("selection workspace is invalid")
		}
	}
	for _, name := range selection.Names {
		if !validName(name) {
			return nil, invalid("selected name is invalid")
		}
	}
	var out []*source
	for _, s := range r.sources {
		if s.config.Scope == harness.SkillScopeGlobal && selection.IncludeGlobal || s.config.Scope == harness.SkillScopeWorkspace && workspace != "" && session.SameWorkspace(workspace, s.config.Workspace) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].config.ID < out[j].config.ID })
	return out, nil
}

// List returns management metadata, including disabled installations. It does
// not read skill bodies or resolve duplicate names across installation sources.
func (r *Registry) List(ctx context.Context, selection harness.SkillSelection) ([]harness.SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	sources, err := r.selectedSources(selection)
	if err != nil {
		return nil, err
	}
	var out []harness.SkillRecord
	for _, s := range sources {
		rows, err := r.db.QueryContext(ctx, `SELECT i.name,i.current_version,i.enabled,v.metadata,v.manifest,v.total_bytes,i.updated_at FROM df_skill_installs i JOIN df_skill_versions v ON v.source_key=i.source_key AND v.name=i.name AND v.version=i.current_version WHERE i.source_key=? ORDER BY i.name`, s.identity)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, version, meta, manifest, updated string
			var enabled int
			var size int64
			if err = rows.Scan(&name, &version, &enabled, &meta, &manifest, &size, &updated); err != nil {
				break
			}
			if !selectedName(selection.Names, name) {
				continue
			}
			var fm metadata
			var files []harness.SkillFileInfo
			if json.Unmarshal([]byte(meta), &fm) != nil || json.Unmarshal([]byte(manifest), &files) != nil {
				err = errors.New("skills metadata is corrupt")
				break
			}
			at, _ := time.Parse(time.RFC3339Nano, updated)
			out = append(out, harness.SkillRecord{Ref: harness.SkillRef{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Version: version, Hash: version}, Description: fm.Description, Scope: s.config.Scope, Workspace: s.config.Workspace, Enabled: enabled != 0, FileCount: len(files), TotalBytes: size, InstalledAt: at, Findings: fm.Findings})
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func selectedName(names []string, name string) bool {
	if names == nil {
		return true
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
