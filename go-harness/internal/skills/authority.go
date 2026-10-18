package skills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (p *PublicationTx) recordSourceStatus(dir, status, version, code string) error {
	_, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_source_status VALUES(?,?,?,?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET directory=excluded.directory,status=excluded.status,observed_version=excluded.observed_version,error_code=excluded.error_code`, p.source.identity, p.name, dir, status, version, code)
	return err
}

// refreshSource never restores files from history. Missing, malformed or
// transaction-owned directories are unavailable to NEW executions.
func (r *Registry) refreshSource(p *PublicationTx, dir string, initial *bool) error {
	head, err := p.Current()
	if err != nil {
		return err
	}
	if head.StateVersion == 0 {
		var guarded bool
		if err = p.tx.QueryRowContext(p.ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_creation_guards WHERE source_key=? AND name=?)`, p.source.identity, p.name).Scan(&guarded); err != nil {
			return err
		}
		if guarded {
			return p.recordSourceStatus(dir, "unavailable", "", "creation_requires_publication")
		}
	}
	if pending, err := editPending(p.ctx, p.tx, p.source.identity, p.name, ""); err != nil {
		return err
	} else if pending {
		return p.recordSourceStatus(dir, "unavailable", "", "edit_reconciliation_required")
	}
	if pending, err := publicationPending(p.ctx, p.tx, p.source.identity, p.name); err != nil {
		return err
	} else if pending {
		return p.recordSourceStatus(dir, "unavailable", "", "publication_reconciliation_required")
	}
	if dir == "" {
		return p.recordSourceStatus(dir, "unavailable", "", "legacy_database_only_requires_migration")
	}
	first, err := r.readPackage(p.ctx, p.source, dir)
	if err != nil || first.meta.Name != p.name {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		code := "source_missing_or_invalid"
		if first.meta.Name != "" && first.meta.Name != p.name {
			code = "source_name_mismatch"
		}
		return p.recordSourceStatus(dir, "unavailable", "", code)
	}
	version := (&Candidate{pkg: first}).Version()
	second, err := r.readPackage(p.ctx, p.source, dir)
	if err != nil || (&Candidate{pkg: second}).Version() != version {
		return p.recordSourceStatus(dir, "unavailable", "", "source_changed_while_reading")
	}
	if _, err = p.stagePackage(first); err != nil {
		return err
	}
	enabled := head.Enabled
	if !head.Managed && initial != nil {
		enabled = *initial
	}
	if !head.Exists || head.Version != version || head.Enabled != enabled {
		if _, err = p.moveHead(head, version, enabled, head.Managed, version, dir); err != nil {
			return err
		}
	} else {
		if _, err = p.tx.ExecContext(p.ctx, `UPDATE df_skill_control SET last_import_version=?,import_directory=? WHERE source_key=? AND name=?`, version, dir, p.source.identity, p.name); err != nil {
			return err
		}
	}
	return p.recordSourceStatus(dir, "ready", version, "")
}

// RefreshSources is an index operation, not a publication. Durable source
// bindings survive disabled/deleted files and remain inspectable after restart.
func (r *Registry) RefreshSources(ctx context.Context, selection harness.SkillSelection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	sources, err := r.selectedSources(selection)
	if err != nil {
		return err
	}
	return r.refreshSourcesLocked(ctx, sources, selection.Names)
}

// RefreshAllSources is a host-only index refresh, including bindings whose
// packages disappeared from startup discovery. It never writes source files.
func (r *Registry) RefreshAllSources(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	var sources []*source
	for _, s := range r.sources {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].identity < sources[j].identity })
	return r.refreshSourcesLocked(ctx, sources, nil)
}

func (r *Registry) refreshSourcesLocked(ctx context.Context, sources []*source, names []string) error {
	for _, s := range sources {
		rows, err := r.db.QueryContext(ctx, `SELECT name,import_directory FROM df_skill_control WHERE source_key=? UNION SELECT name,directory FROM df_skill_source_status WHERE source_key=? AND name NOT IN (SELECT name FROM df_skill_control WHERE source_key=?)`, s.identity, s.identity, s.identity)
		if err != nil {
			return err
		}
		bindings := map[string]string{}
		for rows.Next() {
			var name, dir string
			if err = rows.Scan(&name, &dir); err != nil {
				break
			}
			if selectedName(names, name) {
				bindings[name] = dir
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		ordered := make([]string, 0, len(bindings))
		for name := range bindings {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		for _, name := range ordered {
			err = r.withPublication(ctx, s, name, func(p *PublicationTx) error { return r.refreshSource(p, bindings[name], nil) })
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Registry) installAuthoritative(ctx context.Context, s *source, dir string, enabled *bool) (harness.SkillRecord, error) {
	name := path.Base(dir)
	if dir == "." {
		pkg, err := r.readPackage(ctx, s, dir)
		if err != nil {
			return harness.SkillRecord{}, err
		}
		name = pkg.meta.Name
	}
	if !validName(name) {
		return harness.SkillRecord{}, invalid("package directory must be its skill name")
	}
	var result harness.SkillRecord
	err := r.withPublication(ctx, s, name, func(p *PublicationTx) error {
		var prior string
		err := p.tx.QueryRowContext(ctx, `SELECT import_directory FROM df_skill_control WHERE source_key=? AND name=?`, s.identity, name).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if prior != "" && prior != dir {
			return denied("skill is already bound to a different source directory")
		}
		if err = r.refreshSource(p, dir, enabled); err != nil {
			return err
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if !head.Exists {
			return nil
		}
		entry, err := p.catalogEntry()
		if err != nil {
			return err
		}
		if entry.Record != nil {
			result = *entry.Record
		}
		return nil
	})
	return result, err
}

func (s *Snapshot) SourceWarnings() string { return s.warnings }

func (r *Registry) sourceWarnings(ctx context.Context, sources []*source, names []string) (string, error) {
	var messages []string
	for _, s := range sources {
		rows, err := r.db.QueryContext(ctx, `SELECT st.name,st.directory,st.error_code FROM df_skill_source_status st LEFT JOIN df_skill_installs i ON i.source_key=st.source_key AND i.name=st.name
 WHERE st.source_key=? AND st.status<>'ready' AND (i.enabled=1 OR i.enabled IS NULL)
 AND NOT EXISTS(SELECT 1 FROM df_skill_control c WHERE c.source_key=st.source_key AND c.name=st.name AND c.deleted=1)
 ORDER BY st.name`, s.identity)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var name, dir, code string
			if err = rows.Scan(&name, &dir, &code); err != nil {
				break
			}
			if selectedName(names, name) {
				messages = append(messages, fmt.Sprintf("Skill %s (source %s, directory %s) is unavailable: %s. No cached installation is loaded; use host settings to repair it.", name, s.config.ID, filepath.Join(s.config.Root, filepath.FromSlash(dir)), code))
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return "", err
		}
	}
	return strings.Join(messages, "\n"), nil
}
