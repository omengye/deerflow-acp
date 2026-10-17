package skills

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var (
	ErrPublicationConflict = errors.New("skill publication changed")
	ErrManagedSkill        = errors.New("skill is managed by publications; use an explicit reviewed publication")
)

// Candidate owns a validated, immutable copy of the package. Constructing one
// verifies content structure, not moderation or permission to publish it.
type Candidate struct{ pkg skillPackage }

func (c *Candidate) Version() string {
	manifest, _ := json.Marshal(c.pkg.manifest)
	return digest(manifest)
}
func (c *Candidate) Name() string { return c.pkg.meta.Name }
func (c *Candidate) Findings() []harness.SkillFinding {
	return append([]harness.SkillFinding(nil), c.pkg.meta.Findings...)
}
func (c *Candidate) Files() map[string][]byte {
	files := make(map[string][]byte, len(c.pkg.files))
	for name, data := range c.pkg.files {
		files[name] = bytes.Clone(data)
	}
	return files
}

// PrepareCandidate accepts file contents, never paths to open or scripts to run.
// Evolution adds reference/tool/security checks before making a reviewable offer.
func (r *Registry) PrepareCandidate(name string, files map[string][]byte) (*Candidate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	p, err := packageFromFiles(name, files, r.limits)
	if err != nil {
		return nil, err
	}
	return &Candidate{pkg: p}, nil
}

func packageFromFiles(name string, files map[string][]byte, limits harness.SkillLimits) (skillPackage, error) {
	p := skillPackage{files: make(map[string][]byte, len(files))}
	if !validName(name) || len(files) < 1 || len(files) > limits.MaxFilesPerSkill {
		return p, invalid("candidate name or file count is invalid")
	}
	folded := make(map[string]bool, len(files))
	directories := make(map[string]string)
	for filename, data := range files {
		if !validRelative(filename) || strings.Count(filename, "/") > 15 {
			return p, invalid("candidate contains an invalid path")
		}
		key := strings.ToLower(filename)
		if folded[key] {
			return p, invalid("case-colliding file paths")
		}
		for _, part := range strings.Split(key, "/") {
			if part == ".git" || part == ".svn" {
				return p, invalid("version-control metadata is not a skill file")
			}
		}
		folded[key] = true
		parts := strings.Split(filename, "/")
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/")
			fold := strings.ToLower(dir)
			if prior, exists := directories[fold]; exists && prior != dir {
				return p, invalid("case-colliding directory paths")
			}
			directories[fold] = dir
		}
		if int64(len(data)) > limits.MaxFileBytes || int64(len(data)) > limits.MaxSkillBytes-p.bytes {
			return p, invalid("candidate exceeds file or package byte limit")
		}
		p.files[filename] = bytes.Clone(data)
		p.bytes += int64(len(data))
		p.manifest = append(p.manifest, harness.SkillFileInfo{Path: filename, Hash: digest(data), Bytes: int64(len(data))})
	}
	for filename := range folded {
		parts := strings.Split(filename, "/")
		for i := 1; i < len(parts); i++ {
			if folded[strings.Join(parts[:i], "/")] {
				return p, invalid("candidate path is both a file and directory")
			}
		}
	}
	main, ok := p.files["SKILL.md"]
	if !ok {
		return p, invalid("SKILL.md is required")
	}
	var err error
	p.meta, _, err = parse(main)
	if err != nil {
		return p, err
	}
	if p.meta.Name != name {
		return p, invalid("candidate name differs from SKILL.md")
	}
	sort.Slice(p.manifest, func(i, j int) bool { return p.manifest[i].Path < p.manifest[j].Path })
	p.meta.Findings = scan(p.files, p.manifest)
	return p, nil
}

// PublicationTarget binds a source ID to its immutable configured identity and
// scope. A model-facing caller must separately authorize this target's scope.
type PublicationTarget = harness.SkillTarget

// SkillHead includes a monotonically increasing state version. Comparing only
// a content digest would miss an intervening rollback to that same content.
type SkillHead = harness.SkillHead

// PublicationSources exposes configured identities to the operator service;
// it does not enumerate user directories or grant a model access to them.
func (r *Registry) PublicationSources() ([]harness.SkillTarget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	result := make([]harness.SkillTarget, 0, len(r.sources))
	for _, s := range r.sources {
		result = append(result, harness.SkillTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Scope: s.config.Scope, Workspace: s.config.Workspace})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SourceIdentity < result[j].SourceIdentity })
	return result, nil
}

type PublicationTx struct {
	tx          *sql.Tx
	source      *source
	name        string
	limits      harness.SkillLimits
	target      PublicationTarget
	ctx         context.Context
	export      *exportTarget
	editID      string
	registry    *Registry
	publication *filePublication
}

func (p *PublicationTx) Target() PublicationTarget { return p.target }

// SQL commits proposal, evaluation, history and the file-publication journal
// atomically. Files are verified before a second transaction changes the head.
// Callers must not retain it, commit it or reenter Registry methods from f.
func (p *PublicationTx) SQL() *sql.Tx { return p.tx }

func (r *Registry) WithPublication(ctx context.Context, sourceID, name string, f func(*PublicationTx) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	s := r.sources[sourceID]
	if s == nil {
		return denied("source is not configured")
	}
	if !validName(name) || f == nil {
		return invalid("publication requires a valid name and transaction")
	}
	return r.withPublication(ctx, s, name, f)
}

func (r *Registry) withPublication(ctx context.Context, s *source, name string, f func(*PublicationTx) error) error {
	return r.withEditPublication(ctx, s, name, "", f)
}

func (r *Registry) withEditPublication(ctx context.Context, s *source, name, editID string, f func(*PublicationTx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize before reading the head, also across different Registry objects.
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_catalog SET revision=revision WHERE id=1`); err != nil {
		return err
	}
	p := &PublicationTx{tx: tx, source: s, name: name, limits: r.limits, ctx: ctx, export: r.export, editID: editID, registry: r,
		target: PublicationTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Scope: s.config.Scope, Workspace: s.config.Workspace}}
	if err = f(p); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if p.publication != nil {
		return r.finishPublication(ctx, s, *p.publication)
	}
	return nil
}

func (p *PublicationTx) Current() (SkillHead, error) {
	return ReadPublicationHead(p.ctx, p.tx, p.source.identity, p.name)
}

// ReadPublicationHead reads a complete head inside an already authorized host
// transaction. It does not itself grant access to the source identity.
func ReadPublicationHead(ctx context.Context, tx *sql.Tx, source, name string) (SkillHead, error) {
	var h SkillHead
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT state_version,managed,deleted FROM df_skill_control WHERE source_key=? AND name=?`, source, name).Scan(&h.StateVersion, &h.Managed, &deleted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	control := err == nil
	err = tx.QueryRowContext(ctx, `SELECT current_version,enabled FROM df_skill_installs WHERE source_key=? AND name=?`, source, name).Scan(&h.Version, &h.Enabled)
	if err == nil {
		h.Exists = true
		if !control || deleted || !validHash(h.Version) || h.StateVersion < 1 {
			return SkillHead{}, errors.New("skill installation head is inconsistent")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return h, err
	} else if control && (!deleted || h.StateVersion < 1) {
		return h, errors.New("skill publication tombstone is inconsistent")
	}
	return h, nil
}

// Stage stores the immutable package without changing the installed pointer.
// The proposal owns the retained version until publication or explicit GC.
func (p *PublicationTx) Stage(c *Candidate) (string, error) {
	if c == nil || c.pkg.meta.Name != p.name {
		return "", invalid("candidate belongs to another skill")
	}
	// Registry instances can have different limits. Repeat structural validation
	// within the destination registry before retaining any bytes.
	pkg, err := packageFromFiles(p.name, c.pkg.files, p.limits)
	if err != nil {
		return "", err
	}
	return p.stagePackage(pkg)
}

func (p *PublicationTx) stagePackage(pkg skillPackage) (string, error) {
	manifest, _ := json.Marshal(pkg.manifest)
	version := digest(manifest)
	var exists bool
	if err := p.tx.QueryRowContext(p.ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_versions WHERE source_key=? AND name=? AND version=?)`, p.source.identity, p.name, version).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		if _, err := p.ReadVersion(version); err != nil {
			return "", err
		}
		return version, nil
	}
	var count int
	var total int64
	if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM df_skill_versions WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&count); err != nil {
		return "", err
	}
	if err := p.tx.QueryRowContext(p.ctx, `SELECT COALESCE(sum(total_bytes),0) FROM df_skill_versions`).Scan(&total); err != nil {
		return "", err
	}
	if count >= p.limits.MaxVersionsPerSkill || pkg.bytes > p.limits.MaxTotalBytes-total {
		return "", invalid("retained version or total byte limit exceeded")
	}
	meta, _ := json.Marshal(pkg.meta)
	if _, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_versions VALUES(?,?,?,?,?,?,?)`, p.source.identity, p.name, version, string(meta), string(manifest), pkg.bytes, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	for _, f := range pkg.manifest {
		if err := p.registry.storeObject(pkg.files[f.Path]); err != nil {
			return "", err
		}
		if _, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_files VALUES(?,?,?,?,?,?,?)`, p.source.identity, p.name, version, f.Path, f.Hash, f.Bytes, []byte{}); err != nil {
			return "", err
		}
		if _, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_file_objects VALUES(?,?,?,?)`, p.source.identity, p.name, version, f.Path); err != nil {
			return "", err
		}
	}
	return version, nil
}

// ReadVersion verifies all bytes, including support files, before rollback or
// publication. It never falls back to a mutable source directory.
func (p *PublicationTx) ReadVersion(version string) (*Candidate, error) {
	if !validHash(version) {
		return nil, invalid("version must be a content digest")
	}
	var meta, manifest string
	var size int64
	err := p.tx.QueryRowContext(p.ctx, `SELECT metadata,manifest,total_bytes FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`, p.source.identity, p.name, version).Scan(&meta, &manifest, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	fm, entries, err := decodeVersion(meta, manifest, version, size, p.limits)
	if err != nil || fm.Name != p.name {
		return nil, errors.Join(errors.New("skill publication version metadata is inconsistent"), err)
	}
	files := make(map[string][]byte, len(entries))
	for _, f := range entries {
		data, err := ReadStoredFile(p.ctx, p.tx, p.source.identity, p.name, version, f.Path, p.limits.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != f.Bytes || digest(data) != f.Hash {
			return nil, errors.New("skill publication file digest differs from manifest")
		}
		files[f.Path] = data
	}
	var count int
	if err = p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM df_skill_files WHERE source_key=? AND name=? AND version=?`, p.source.identity, p.name, version).Scan(&count); err != nil {
		return nil, err
	}
	if count != len(entries) {
		return nil, errors.New("skill publication contains unlisted files")
	}
	pkg, err := packageFromFiles(p.name, files, p.limits)
	if err != nil {
		return nil, err
	}
	canonicalMeta, _ := json.Marshal(pkg.meta)
	actualMeta, _ := json.Marshal(fm)
	if !bytes.Equal(canonicalMeta, actualMeta) {
		return nil, errors.New("skill publication metadata differs from content")
	}
	return &Candidate{pkg: pkg}, nil
}

// Publish admits a durable file publication; the head changes only after the
// bound directory is installed and verified. An empty version removes that
// directory into a backup. History remains available for explicit restore.
func (p *PublicationTx) Publish(version string, expected SkillHead, enabled bool) (SkillHead, error) {
	h, err := p.Current()
	if err != nil {
		return h, err
	}
	if h != expected {
		return h, ErrPublicationConflict
	}
	if version != "" {
		if _, err = p.ReadVersion(version); err != nil {
			return h, err
		}
	}
	return p.preparePublication(version, h, enabled)
}

func (p *PublicationTx) moveHead(old SkillHead, version string, enabled, managed bool, imported, dir string) (SkillHead, error) {
	if pending, err := editPending(p.ctx, p.tx, p.source.identity, p.name, p.editID); err != nil {
		return old, err
	} else if pending {
		return old, harness.ErrReconciliationRequired
	}
	if pending, err := publicationPending(p.ctx, p.tx, p.source.identity, p.name); err != nil {
		return old, err
	} else if pending {
		return old, harness.ErrReconciliationRequired
	}
	if version == "" && enabled {
		return old, invalid("deleted skill cannot be enabled")
	}
	if version != "" && !old.Exists {
		var count int
		if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM df_skill_installs`).Scan(&count); err != nil {
			return old, err
		}
		if count >= p.limits.MaxSkills {
			return old, invalid("installed skill count limit exceeded")
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if version == "" {
		if _, err := p.tx.ExecContext(p.ctx, `DELETE FROM df_skill_installs WHERE source_key=? AND name=?`, p.source.identity, p.name); err != nil {
			return old, err
		}
	} else if _, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_installs VALUES(?,?,?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET current_version=excluded.current_version,enabled=excluded.enabled,updated_at=excluded.updated_at`, p.source.identity, p.name, version, enabled, now); err != nil {
		return old, err
	}
	_, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_control(source_key,name,state_version,managed,deleted,last_import_version,import_directory) VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET state_version=excluded.state_version,managed=excluded.managed,deleted=excluded.deleted,last_import_version=CASE WHEN excluded.last_import_version='' THEN df_skill_control.last_import_version ELSE excluded.last_import_version END,import_directory=CASE WHEN excluded.import_directory='' THEN df_skill_control.import_directory ELSE excluded.import_directory END`, p.source.identity, p.name, old.StateVersion+1, managed, version == "", imported, dir)
	if err != nil {
		return old, err
	}
	if _, err = p.tx.ExecContext(p.ctx, `UPDATE df_skill_catalog SET revision=revision+1 WHERE id=1`); err != nil {
		return old, err
	}
	next := SkillHead{Version: version, StateVersion: old.StateVersion + 1, Exists: version != "", Enabled: enabled, Managed: managed}
	if err = p.queueExport(next); err != nil {
		return old, err
	}
	return next, nil
}

func (p *PublicationTx) CatalogVersion() (int64, error) {
	var version int64
	err := p.tx.QueryRowContext(p.ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&version)
	return version, err
}

// PublicationStore is shared with trusted host services so their proposal and
// publication records use the registry's existing database and transaction.
// The registry retains ownership; the caller must not close it.
func (r *Registry) PublicationStore() *sql.DB { return r.db }
