package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const MaxMatchInventory = 128
const MaxMatchBodyBytes = 32 << 10
const maxMatchMetadataBytes = 128 << 10
const maxMatchInventoryBytes = 2 << 20

type matchSource struct {
	target  harness.SkillTarget
	builtin bool
}

// MatchAccess copies startup host policy, not Registry locks or directory
// handles. Its reads use the caller's transaction: the SQLite pool can have a
// single connection, and acquiring Registry locks here would invert publication
// lock order. Workspace must be the canonical, host-stored session CWD.
type MatchAccess struct {
	sources  []matchSource
	names    []string
	global   bool
	limits   harness.SkillLimits
	identity string
}

type MatchSkill struct {
	Target      harness.SkillTarget `json:"target"`
	Head        harness.SkillHead   `json:"head"`
	OverlayBase *harness.SkillRef   `json:"overlay_base,omitempty"`
	Description string              `json:"description"`
	Builtin     bool                `json:"builtin"`
}

type MatchInventory struct {
	Catalog     int64        `json:"catalog_revision"`
	Skills      []MatchSkill `json:"skills"`
	access      *MatchAccess
	transaction *sql.Tx
	grants      map[string]bool
}

func (r *Registry) NewMatchAccess(selection harness.SkillSelection) (*MatchAccess, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	for _, name := range selection.Names {
		if !validName(name) {
			return nil, invalid("matching selection name is invalid")
		}
	}
	a := &MatchAccess{names: slices.Clone(selection.Names), global: selection.IncludeGlobal, limits: r.limits}
	for _, s := range r.sources {
		a.sources = append(a.sources, matchSource{target: sourceTarget(s, ""), builtin: s.builtin})
	}
	sort.Slice(a.sources, func(i, j int) bool { return a.sources[i].target.SourceID < a.sources[j].target.SourceID })
	data, _ := json.Marshal([]any{"host-skill-match-access-v1", a.sourcesProjection(), a.names, a.global, a.limits})
	a.identity = digest(data)
	return a, nil
}

func (a *MatchAccess) sourcesProjection() []any {
	result := make([]any, 0, len(a.sources))
	for _, s := range a.sources {
		result = append(result, []any{s.target, s.builtin})
	}
	return result
}

func (a *MatchAccess) Identity() string { return a.identity }

// NewCapabilityTargetTx is host source arbitration, not model authority. A
// restrictive Names selection or disabled/tombstoned visible name must never be
// bypassed by creating a fresh skill, even in a higher-precedence source.
func (a *MatchAccess) NewCapabilityTargetTx(ctx context.Context, tx *sql.Tx, workspace, sourceID, name string) (harness.SkillTarget, error) {
	var target harness.SkillTarget
	if a == nil || tx == nil || a.names != nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || !harness.ValidSkillCapabilityName(name) {
		return target, denied("new capability requires unrestricted host skill selection")
	}
	for _, s := range a.sources {
		visible := s.target.Scope == harness.SkillScopeGlobal && a.global || s.target.Scope == harness.SkillScopeWorkspace && session.SameWorkspace(workspace, s.target.Workspace)
		if !visible {
			continue
		}
		if s.target.SourceID == sourceID && !s.builtin {
			target = s.target
			target.Name = name
		}
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_control WHERE source_key=? AND name=?)
 OR EXISTS(SELECT 1 FROM df_skill_installs WHERE source_key=? AND name=?)`, s.target.SourceIdentity, name, s.target.SourceIdentity, name).Scan(&occupied); err != nil {
			return harness.SkillTarget{}, err
		}
		if occupied {
			return harness.SkillTarget{}, harness.ErrSkillConflict
		}
	}
	if target.SourceID == "" {
		return target, denied("new capability source is not visible and non-builtin")
	}
	return target, nil
}

// InventoryTx resolves the same disabled-shadow and duplicate-name semantics
// as a new runtime Snapshot. Management List is deliberately not an access list.
func (a *MatchAccess) InventoryTx(ctx context.Context, tx *sql.Tx, workspace string) (MatchInventory, error) {
	inventory := MatchInventory{Skills: []MatchSkill{}, access: a, transaction: tx, grants: map[string]bool{}}
	if a == nil || tx == nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return inventory, denied("matching requires a canonical host workspace")
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&inventory.Catalog); err != nil {
		return inventory, err
	}
	if inventory.Catalog < 0 {
		return inventory, invalid("matching catalog revision is invalid")
	}
	chosen := map[string]MatchSkill{}
	if a.names != nil && len(a.names) == 0 {
		return inventory, nil
	}
	scanned, metadataBytes := 0, 0
	for _, s := range a.sources {
		if s.target.Scope == harness.SkillScopeGlobal && !a.global || s.target.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(workspace, s.target.Workspace) {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT i.name,i.current_version,i.enabled,
 CASE WHEN length(CAST(v.metadata AS BLOB))+length(CAST(v.manifest AS BLOB))<=? THEN v.metadata ELSE '' END,
 CASE WHEN length(CAST(v.metadata AS BLOB))+length(CAST(v.manifest AS BLOB))<=? THEN v.manifest ELSE '' END,v.total_bytes
 FROM df_skill_installs i JOIN df_skill_versions v ON v.source_key=i.source_key AND v.name=i.name AND v.version=i.current_version
 WHERE i.source_key=? ORDER BY i.name LIMIT ?`, maxMatchMetadataBytes, maxMatchMetadataBytes, s.target.SourceIdentity, MaxMatchInventory+1)
		if err != nil {
			return inventory, err
		}
		count := 0
		for rows.Next() {
			var name, version, meta, manifest string
			var enabled bool
			var size int64
			if err = rows.Scan(&name, &version, &enabled, &meta, &manifest, &size); err != nil {
				break
			}
			count++
			scanned++
			metadataBytes += len(meta) + len(manifest)
			if count > MaxMatchInventory || scanned > 2*MaxMatchInventory || metadataBytes > maxMatchInventoryBytes {
				err = invalid("matching inventory exceeds bounds")
				break
			}
			if !selectedName(a.names, name) {
				continue
			}
			fm, _, decodeErr := decodeVersion(meta, manifest, version, size, a.limits)
			if decodeErr != nil || fm.Name != name {
				err = invalid("matching version metadata is inconsistent")
				break
			}
			target := s.target
			target.Name = name
			e := MatchSkill{Target: target, Head: harness.SkillHead{Version: version, Enabled: enabled}, Description: fm.Description, Builtin: s.builtin}
			if prior, ok := chosen[name]; ok {
				if prior.Target.Scope == e.Target.Scope {
					if prior.Head.Enabled && e.Head.Enabled {
						err = &harness.SkillNameConflictError{Name: name, FirstSource: prior.Target.SourceID, SecondSource: e.Target.SourceID}
						break
					}
					if prior.Head.Enabled || !e.Head.Enabled {
						continue
					}
				}
				if prior.Target.Scope != e.Target.Scope && prior.Target.Scope == harness.SkillScopeWorkspace {
					continue
				}
			}
			chosen[name] = e
			if len(chosen) > MaxMatchInventory || len(chosen) > a.limits.MaxSkills {
				err = invalid("matching inventory exceeds bounds")
				break
			}
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return inventory, err
		}
	}
	for name, e := range chosen {
		if !e.Head.Enabled {
			continue
		}
		head, err := ReadPublicationHead(ctx, tx, e.Target.SourceIdentity, name)
		if err != nil {
			return inventory, err
		}
		if !head.Exists || !head.Enabled || head.Version != e.Head.Version {
			return inventory, invalid("matching head is inconsistent")
		}
		e.Head = head
		var base string
		err = tx.QueryRowContext(ctx, `SELECT base_version FROM df_skill_overlays WHERE source_key=? AND name=?`, e.Target.SourceIdentity, name).Scan(&base)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return inventory, err
		}
		if err == nil {
			if !head.Managed || !validHash(base) {
				return inventory, invalid("matching overlay provenance is inconsistent")
			}
			e.OverlayBase = &harness.SkillRef{SourceID: e.Target.SourceID, SourceIdentity: e.Target.SourceIdentity, Name: name, Version: base, Hash: base}
			e.Builtin = true
		}
		inventory.Skills = append(inventory.Skills, e)
	}
	sort.Slice(inventory.Skills, func(i, j int) bool { return inventory.Skills[i].Target.Name < inventory.Skills[j].Target.Name })
	for _, name := range a.names {
		if !slices.ContainsFunc(inventory.Skills, func(e MatchSkill) bool { return e.Target.Name == name }) {
			return inventory, harness.ErrNotFound
		}
	}
	for _, e := range inventory.Skills {
		inventory.grants[matchDescriptorKey(e)] = true
	}
	return inventory, nil
}

func matchDescriptorKey(e MatchSkill) string {
	data, _ := json.Marshal(e)
	return digest(data)
}

// BodyTx never scans scripts, support files or mutable source directories. The
// private grant admits only the exact descriptor from this access/transaction,
// even if a caller mutates the public inventory or reuses an old transaction.
func (a *MatchAccess) BodyTx(ctx context.Context, tx *sql.Tx, inventory MatchInventory, e MatchSkill) (string, error) {
	if a == nil || tx == nil || inventory.access != a || inventory.transaction != tx || !inventory.grants[matchDescriptorKey(e)] {
		return "", denied("matching body requires a live authorized inventory grant")
	}
	var meta, manifest string
	var total int64
	err := tx.QueryRowContext(ctx, `SELECT metadata,manifest,total_bytes FROM df_skill_versions WHERE source_key=? AND name=? AND version=?
 AND length(CAST(metadata AS BLOB))+length(CAST(manifest AS BLOB))<=?`, e.Target.SourceIdentity, e.Target.Name, e.Head.Version, maxMatchMetadataBytes).Scan(&meta, &manifest, &total)
	if err != nil {
		return "", err
	}
	fm, files, err := decodeVersion(meta, manifest, e.Head.Version, total, a.limits)
	if err != nil || fm.Name != e.Target.Name || fm.Description != e.Description {
		return "", invalid("matching body metadata is inconsistent")
	}
	index := slices.IndexFunc(files, func(f harness.SkillFileInfo) bool { return f.Path == "SKILL.md" })
	if index < 0 || files[index].Bytes > MaxMatchBodyBytes {
		return "", invalid("matching SKILL.md exceeds bounds")
	}
	f := files[index]
	data, err := ReadStoredFile(ctx, tx, e.Target.SourceIdentity, e.Target.Name, e.Head.Version, "SKILL.md", MaxMatchBodyBytes)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != f.Bytes || digest(data) != f.Hash {
		return "", invalid("matching immutable body failed integrity verification")
	}
	fm, body, err := parse(data)
	if err != nil || fm.Name != e.Target.Name || fm.Description != e.Description {
		return "", invalid("matching body differs from its metadata")
	}
	return body, nil
}
