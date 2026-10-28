package skills

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func sourceTarget(s *source, name string) harness.SkillTarget {
	return harness.SkillTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Scope: s.config.Scope, Workspace: s.config.Workspace}
}

type catalogCursor struct {
	Kind, Filter, Name, Source string
	Revision                   int64
}

// QueryCatalog pages only configured source identities. A cursor pins the
// publication revision and filters; advisory exporter progress does not expire it.
func (r *Registry) QueryCatalog(ctx context.Context, q harness.SkillQuery) (harness.SkillCatalogPage, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	page := harness.SkillCatalogPage{Skills: []harness.SkillCatalogEntry{}, Sources: []harness.SkillCatalogSource{}}
	page.Diagnostics = append([]harness.SkillFinding(nil), r.diagnostics...)
	if r.closed {
		return page, ErrClosed
	}
	if q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 2048 || len(q.SourceID) > 128 || q.Name != "" && !validName(q.Name) {
		return page, invalid("invalid catalog query")
	}
	if len(q.NameContains) > 128 || !utf8.ValidString(q.NameContains) || strings.ContainsRune(q.NameContains, 0) {
		return page, invalid("invalid catalog name substring")
	}
	q.NameContains = strings.ToLower(strings.TrimSpace(q.NameContains))
	if q.Status != "" && q.Status != "current" && q.Status != "enabled" && q.Status != "disabled" && q.Status != "deleted" {
		return page, invalid("catalog status must be current, enabled, disabled or deleted")
	}
	if q.SourceID != "" && r.sources[q.SourceID] == nil {
		return page, harness.ErrPermissionDenied
	}
	allowed := map[string]*source{}
	for _, s := range r.sources {
		if q.SourceID == "" || q.SourceID == s.config.ID {
			allowed[s.identity] = s
			page.Sources = append(page.Sources, harness.SkillCatalogSource{Target: sourceTarget(s, ""), Builtin: s.builtin, ManagedSource: s.managed})
		}
	}
	sort.Slice(page.Sources, func(i, j int) bool {
		return page.Sources[i].Target.SourceIdentity < page.Sources[j].Target.SourceIdentity
	})
	exportKey := ""
	if r.export != nil {
		exportKey = r.export.key
	}
	filter, _ := json.Marshal([]any{page.Sources, q.Name, q.Status, exportKey})
	cursor := catalogCursor{Kind: "skill-catalog-v1", Filter: digest(filter)}
	if q.NameContains != "" {
		// Keep existing exact-name/list cursors valid when no substring is requested.
		filter, _ = json.Marshal([]any{cursor.Filter, map[string]string{"name_contains": q.NameContains}})
		cursor.Filter = digest(filter)
	}
	if q.Cursor != "" {
		var saved catalogCursor
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &saved) != nil || saved.Kind != cursor.Kind || saved.Filter != cursor.Filter || !validName(saved.Name) || allowed[saved.Source] == nil || saved.Revision < 0 {
			return page, invalid("catalog cursor does not match query")
		}
		cursor = saved
	}
	args := make([]any, 0, len(page.Sources)+6)
	for _, s := range page.Sources {
		args = append(args, s.Target.SourceIdentity)
	}
	where := "0=1"
	if len(args) > 0 {
		where = "c.source_key IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	}
	if q.Name != "" {
		where += " AND c.name=?"
		args = append(args, q.Name)
	}
	if q.NameContains != "" {
		where += " AND instr(c.name, ?)>0"
		args = append(args, q.NameContains)
	}
	switch q.Status {
	case "current":
		where += " AND c.deleted=0"
	case "enabled":
		where += " AND c.deleted=0 AND i.enabled=1"
	case "disabled":
		where += " AND c.deleted=0 AND i.enabled=0"
	case "deleted":
		where += " AND c.deleted=1"
	}
	where += " AND (c.name>? OR (c.name=? AND c.source_key>?))"
	args = append(args, cursor.Name, cursor.Name, cursor.Source, q.Limit+1)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&page.CatalogVersion); err != nil {
		return page, err
	}
	if q.Cursor != "" && cursor.Revision != page.CatalogVersion {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = page.CatalogVersion
	rows, err := tx.QueryContext(ctx, `SELECT c.source_key,c.name FROM df_skill_control c LEFT JOIN df_skill_installs i ON i.source_key=c.source_key AND i.name=c.name WHERE `+where+` ORDER BY c.name,c.source_key LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	type key struct{ source, name string }
	var keys []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.source, &k.name); err != nil {
			break
		}
		keys = append(keys, k)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close() // The shared pool can contain just one connection.
	if err != nil {
		return page, err
	}
	base, _ := json.Marshal(page)
	bytesUsed := len(base)
	for _, k := range keys {
		if len(page.Skills) == q.Limit {
			page.Next = encodeCatalogCursor(cursor)
			break
		}
		s := allowed[k.source]
		if s == nil || !validName(k.name) {
			return page, errors.New("catalog identity is inconsistent")
		}
		p := r.catalogTx(ctx, tx, s, k.name)
		entry, err := p.catalogEntry()
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return page, err
		}
		if bytesUsed+len(encoded) > 2<<20 {
			if len(page.Skills) == 0 {
				return page, invalid("catalog entry exceeds response limit")
			}
			page.Next = encodeCatalogCursor(cursor)
			break
		}
		bytesUsed += len(encoded) + 1
		page.Skills = append(page.Skills, entry)
		cursor.Name, cursor.Source = k.name, k.source
	}
	return page, tx.Commit()
}

func encodeCatalogCursor(c catalogCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func (r *Registry) catalogTx(ctx context.Context, tx *sql.Tx, s *source, name string) *PublicationTx {
	return &PublicationTx{tx: tx, source: s, name: name, limits: r.limits, target: sourceTarget(s, name), ctx: ctx, export: r.export, registry: r}
}

func (p *PublicationTx) catalogEntry() (harness.SkillCatalogEntry, error) {
	entry := harness.SkillCatalogEntry{Target: p.target, Builtin: p.source.builtin, ManagedSource: p.source.managed}
	var err error
	entry.Head, err = p.Current()
	if err != nil {
		return entry, err
	}
	if entry.Head.StateVersion == 0 {
		return entry, harness.ErrNotFound
	}
	var dir string
	err = p.tx.QueryRowContext(p.ctx, `SELECT directory,status,error_code FROM df_skill_source_status WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&dir, &entry.SourceStatus, &entry.SourceError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return entry, err
	}
	if dir != "" {
		entry.SourceDirectory = filepath.Join(p.source.config.Root, filepath.FromSlash(dir))
	}
	if entry.SourceStatus == "" {
		entry.SourceStatus = "not_observed"
	}
	if pending, e := publicationPending(p.ctx, p.tx, p.source.identity, p.name); e != nil {
		return entry, e
	} else if pending {
		entry.SourceStatus, entry.SourceError = "unavailable", "publication_reconciliation_required"
	}
	if pending, e := editPending(p.ctx, p.tx, p.source.identity, p.name, ""); e != nil {
		return entry, e
	} else if pending {
		entry.SourceStatus, entry.SourceError = "unavailable", "edit_reconciliation_required"
	}
	var overlay string
	err = p.tx.QueryRowContext(p.ctx, `SELECT base_version FROM df_skill_overlays WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&overlay)
	if err == nil {
		if !validHash(overlay) || !entry.Head.Managed {
			return entry, errors.New("skill overlay provenance is inconsistent")
		}
		entry.OverlayBase = &harness.SkillRef{SourceID: p.target.SourceID, SourceIdentity: p.target.SourceIdentity, Name: p.name, Version: overlay, Hash: overlay}
		entry.Builtin = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return entry, err
	}
	if entry.Head.Exists {
		var meta, manifest, updated string
		var size int64
		err = p.tx.QueryRowContext(p.ctx, `SELECT v.metadata,v.manifest,v.total_bytes,i.updated_at FROM df_skill_installs i JOIN df_skill_versions v ON v.source_key=i.source_key AND v.name=i.name AND v.version=i.current_version WHERE i.source_key=? AND i.name=?`, p.source.identity, p.name).Scan(&meta, &manifest, &size, &updated)
		if err != nil {
			return entry, err
		}
		fm, files, err := decodeVersion(meta, manifest, entry.Head.Version, size, p.limits)
		if err != nil {
			return entry, err
		}
		at, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil || fm.Name != p.name {
			return entry, errors.New("catalog metadata is inconsistent")
		}
		entry.Record = &harness.SkillRecord{Ref: harness.SkillRef{SourceID: p.target.SourceID, SourceIdentity: p.target.SourceIdentity, Name: p.name, Version: entry.Head.Version, Hash: entry.Head.Version}, Description: fm.Description, Scope: p.target.Scope, Workspace: p.target.Workspace, Enabled: entry.Head.Enabled, FileCount: len(files), TotalBytes: size, InstalledAt: at, Findings: fm.Findings, Builtin: entry.Builtin, Managed: entry.Head.Managed, StateVersion: entry.Head.StateVersion, OverlayBase: entry.OverlayBase}
	}
	if err = p.tx.QueryRowContext(p.ctx, `SELECT import_directory<>'' FROM df_skill_control WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&entry.CanRefreshImport); err != nil {
		return entry, err
	}
	if entry.CanRefreshImport {
		var version, payload string
		var checked int64
		err = p.tx.QueryRowContext(p.ctx, `SELECT o.import_version,o.checked_at_ns,o.payload FROM df_skill_import_observations o JOIN df_skill_control c ON c.source_key=o.source_key AND c.name=o.name AND c.last_import_version=o.import_version AND c.import_directory=o.import_directory WHERE o.source_key=? AND o.name=?`, p.source.identity, p.name).Scan(&version, &checked, &payload)
		if err == nil {
			entry.Import, err = decodeImportObservation(payload, p.target, version, checked)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return entry, err
		}
	}
	if p.export != nil && entry.Head.Managed {
		item, err := readExport(p.tx.QueryRowContext(p.ctx, `SELECT `+exportColumns+` FROM df_skill_exports WHERE source_key=? AND name=? AND export_key=?`, p.source.identity, p.name, p.export.key), p.source, p.export.key)
		if err == nil {
			if item.state.Head != entry.Head {
				if pending, e := readFilePublication(p.ctx, p.tx, p.source.identity, p.name); e == nil && pending.Base == entry.Head && pending.Next == item.state.Head {
					return entry, nil // Predicted export belongs to an incomplete file commit.
				}
				return entry, errors.New("catalog export head is inconsistent")
			}
			entry.Export = &item.state
		} else if !errors.Is(err, sql.ErrNoRows) {
			return entry, err
		}
	}
	return entry, nil
}

func (r *Registry) GetCatalogSkill(ctx context.Context, target harness.SkillTarget) (harness.SkillCatalogDetail, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result harness.SkillCatalogDetail
	if r.closed {
		return result, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return result, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	p := r.catalogTx(ctx, tx, s, target.Name)
	if result.Skill, err = p.catalogEntry(); err != nil {
		return result, err
	}
	if result.CatalogVersion, err = p.CatalogVersion(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// Called with r.mu held. Compare full identity, including workspace and scope,
// so a source ID reused for another directory cannot accept an old UI action.
func (r *Registry) catalogTarget(target harness.SkillTarget) (*source, error) {
	if !validName(target.Name) || !validHash(target.SourceIdentity) {
		return nil, invalid("invalid catalog target")
	}
	s := r.sources[target.SourceID]
	if s == nil || sourceTarget(s, target.Name) != target {
		return nil, harness.ErrPermissionDenied
	}
	return s, nil
}
