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
	builtin  bool
	managed  bool
}
type Registry struct {
	mu           sync.RWMutex
	db           *sql.DB
	sources      map[string]*source
	limits       harness.SkillLimits
	closed       bool
	export       *exportTarget
	exportMu     sync.Mutex
	exportCancel context.CancelFunc
	exportDone   chan struct{}
	closeDone    chan struct{}
	closeErr     error
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
	r := &Registry{db: db, sources: make(map[string]*source), limits: limits, closeDone: make(chan struct{})}
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
	for _, id := range config.BuiltinSources {
		s := r.sources[id]
		if s == nil || s.builtin {
			_ = r.Close()
			return nil, invalid("builtin sources must be unique configured source IDs")
		}
		s.builtin = true
	}
	for _, id := range config.ManagedSources {
		s := r.sources[id]
		if s == nil || s.managed || s.builtin {
			_ = r.Close()
			return nil, invalid("managed sources must be unique configured non-builtin source IDs")
		}
		s.managed = true
	}
	if config.ExportRoot != "" {
		var err error
		r.export, err = openExportTarget(config.ExportRoot)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	if len(config.ManagedSources) > 0 && r.export == nil {
		_ = r.Close()
		return nil, invalid("managed sources require an explicit export root")
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
	if r.closed {
		r.mu.Unlock()
		<-r.closeDone
		return r.closeErr
	}
	r.closed = true
	if r.exportCancel != nil {
		r.exportCancel()
	}
	done := r.exportDone
	r.mu.Unlock()
	if done != nil {
		<-done
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	for _, s := range r.sources {
		err = errors.Join(err, s.root.Close())
	}
	if r.export != nil {
		err = errors.Join(err, r.export.root.Close())
	}
	r.closeErr = err
	close(r.closeDone)
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
		`CREATE TABLE IF NOT EXISTS df_skill_catalog(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL CHECK(revision>=0))`,
		`INSERT OR IGNORE INTO df_skill_catalog VALUES(1,0)`,
		`CREATE TABLE IF NOT EXISTS df_skill_control(source_key TEXT NOT NULL,name TEXT NOT NULL,state_version INTEGER NOT NULL CHECK(state_version>0),managed INTEGER NOT NULL,deleted INTEGER NOT NULL,last_import_version TEXT NOT NULL,import_directory TEXT NOT NULL,PRIMARY KEY(source_key,name))`,
		`INSERT OR IGNORE INTO df_skill_control SELECT source_key,name,1,0,0,current_version,'' FROM df_skill_installs`,
		`CREATE TABLE IF NOT EXISTS df_skill_overlays(source_key TEXT NOT NULL,name TEXT NOT NULL,base_version TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(source_key,name),FOREIGN KEY(source_key,name,base_version) REFERENCES df_skill_versions(source_key,name,version))`,
		`CREATE TABLE IF NOT EXISTS df_skill_import_observations(source_key TEXT NOT NULL,name TEXT NOT NULL,import_version TEXT NOT NULL,import_directory TEXT NOT NULL,checked_at_ns INTEGER NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(source_key,name))`,
		`CREATE TABLE IF NOT EXISTS df_skill_exports(source_key TEXT NOT NULL,name TEXT NOT NULL,export_key TEXT NOT NULL,state_version INTEGER NOT NULL,head TEXT NOT NULL,status TEXT NOT NULL,view_directory TEXT NOT NULL,attempts INTEGER NOT NULL,error_code TEXT NOT NULL,updated_at TEXT NOT NULL,retry_after_ns INTEGER NOT NULL,PRIMARY KEY(source_key,name,export_key))`,
		`CREATE TABLE IF NOT EXISTS df_skill_management_requests(source_key TEXT NOT NULL,name TEXT NOT NULL,request_key TEXT NOT NULL,operation TEXT NOT NULL,intent_hash TEXT NOT NULL,intent TEXT NOT NULL,applied_head TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(source_key,name,request_key))`,
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
	return r.install(ctx, sourceID, relativeDir, nil)
}

// InstallConfigured applies a startup import only while the installation is
// externally managed. Published versions, enablement and tombstones survive
// restart, including when the old source directory has disappeared.
func (r *Registry) InstallConfigured(ctx context.Context, sourceID, relativeDir string, enabled bool) error {
	_, err := r.install(ctx, sourceID, relativeDir, &enabled)
	return err
}

func (r *Registry) install(ctx context.Context, sourceID, relativeDir string, startupEnabled *bool) (harness.SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return harness.SkillRecord{}, ErrClosed
	}
	s := r.sources[sourceID]
	if s == nil {
		return harness.SkillRecord{}, denied("source is not configured")
	}
	if s.managed {
		return harness.SkillRecord{}, denied("managed sources accept reviewed publications, never filesystem imports")
	}
	if relativeDir != "." && !validRelative(relativeDir) {
		return harness.SkillRecord{}, invalid("installation directory must be a safe relative path")
	}
	if startupEnabled != nil {
		var managed bool
		if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_control WHERE source_key=? AND import_directory=? AND managed=1)`, s.identity, relativeDir).Scan(&managed); err != nil {
			return harness.SkillRecord{}, err
		}
		if managed {
			return harness.SkillRecord{}, nil
		}
	}
	p, err := r.readPackage(ctx, s, relativeDir)
	if err != nil {
		return harness.SkillRecord{}, err
	}
	var head SkillHead
	err = r.withPublication(ctx, s, p.meta.Name, func(tx *PublicationTx) error {
		var err error
		head, err = tx.Current()
		if err != nil {
			return err
		}
		if head.Managed {
			if startupEnabled != nil {
				return nil
			}
			return ErrManagedSkill
		}
		version, err := tx.stagePackage(p)
		if err != nil {
			return err
		}
		enabled := head.Enabled
		if startupEnabled != nil {
			enabled = *startupEnabled
		}
		if head.Version != version || head.Enabled != enabled {
			head, err = tx.moveHead(head, version, enabled, false, version, relativeDir)
			return err
		}
		// Record the first configured directory after upgrading a legacy DB.
		_, err = tx.tx.ExecContext(ctx, `UPDATE df_skill_control SET last_import_version=?,import_directory=? WHERE source_key=? AND name=?`, version, relativeDir, s.identity, p.meta.Name)
		return err
	})
	if err != nil {
		return harness.SkillRecord{}, err
	}
	return harness.SkillRecord{Ref: harness.SkillRef{SourceID: sourceID, SourceIdentity: s.identity, Name: p.meta.Name, Version: head.Version, Hash: head.Version}, Description: p.meta.Description, Scope: s.config.Scope, Workspace: s.config.Workspace, Enabled: head.Enabled, FileCount: len(p.files), TotalBytes: p.bytes, InstalledAt: time.Now().UTC(), Findings: p.meta.Findings, Builtin: s.builtin, Managed: head.Managed, StateVersion: head.StateVersion}, nil
}

func (r *Registry) SetEnabled(ctx context.Context, sourceID, name string, enabled bool) error {
	return r.WithPublication(ctx, sourceID, name, func(tx *PublicationTx) error {
		h, err := tx.Current()
		if err != nil {
			return err
		}
		if !h.Exists {
			return harness.ErrNotFound
		}
		if h.Enabled == enabled {
			return nil
		}
		_, err = tx.moveHead(h, h.Version, enabled, h.Managed, "", "")
		return err
	})
}
func (r *Registry) Delete(ctx context.Context, sourceID, name string) error {
	return r.WithPublication(ctx, sourceID, name, func(tx *PublicationTx) error {
		h, err := tx.Current()
		if err != nil {
			return err
		}
		if !h.Exists {
			return harness.ErrNotFound
		}
		_, err = tx.moveHead(h, "", false, h.Managed, "", "")
		return err
	})
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
		rows, err := r.db.QueryContext(ctx, `SELECT i.name,i.current_version,i.enabled,v.metadata,v.manifest,v.total_bytes,i.updated_at,c.managed,c.state_version,COALESCE(o.base_version,'') FROM df_skill_installs i JOIN df_skill_versions v ON v.source_key=i.source_key AND v.name=i.name AND v.version=i.current_version JOIN df_skill_control c ON c.source_key=i.source_key AND c.name=i.name LEFT JOIN df_skill_overlays o ON o.source_key=i.source_key AND o.name=i.name WHERE i.source_key=? ORDER BY i.name`, s.identity)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, version, meta, manifest, updated string
			var enabled int
			var size int64
			var managed bool
			var stateVersion int64
			var overlayVersion string
			if err = rows.Scan(&name, &version, &enabled, &meta, &manifest, &size, &updated, &managed, &stateVersion, &overlayVersion); err != nil {
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
			var overlay *harness.SkillRef
			if overlayVersion != "" {
				if !validHash(overlayVersion) || !managed {
					err = errors.New("skill overlay provenance is inconsistent")
					break
				}
				overlay = &harness.SkillRef{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Version: overlayVersion, Hash: overlayVersion}
			}
			out = append(out, harness.SkillRecord{Ref: harness.SkillRef{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Version: version, Hash: version}, Description: fm.Description, Scope: s.config.Scope, Workspace: s.config.Workspace, Enabled: enabled != 0, FileCount: len(files), TotalBytes: size, InstalledAt: at, Findings: fm.Findings, Builtin: s.builtin || overlay != nil, Managed: managed, StateVersion: stateVersion, OverlayBase: overlay})
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
