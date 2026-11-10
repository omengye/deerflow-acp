package skills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// unregisteredFindings is a bounded directory inventory, never an import. In
// particular these observations must NOT enter df_skill_source_status, whose
// rows are durable installation bindings refreshed by refreshSource.
// The caller holds r.mu and must not hold a transaction on r.db.
func (r *Registry) unregisteredFindings(ctx context.Context, sources []*source, names []string) ([]harness.SkillFinding, error) {
	var findings []harness.SkillFinding
	for _, s := range sources {
		if !s.managed {
			continue
		}
		known := map[string]bool{}
		rows, err := r.db.QueryContext(ctx, `SELECT name FROM df_skill_control WHERE source_key=?
 UNION SELECT name FROM df_skill_file_publications WHERE source_key=?
 UNION SELECT name FROM df_skill_creation_guards WHERE source_key=?`, s.identity, s.identity, s.identity)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			known[name] = true
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		dir, err := s.root.Open(".")
		if err != nil {
			return nil, err
		}
		count := 0
		for {
			entries, readErr := dir.ReadDir(128)
			for _, entry := range entries {
				if err = ctx.Err(); err != nil {
					_ = dir.Close()
					return nil, err
				}
				count++
				if count > 4096 {
					findings = append(findings, harness.SkillFinding{Code: "source_inventory_incomplete", Path: s.config.Root, Message: "Skill directory inventory exceeded 4096 entries; host maintenance is required."})
					break
				}
				name := entry.Name()
				if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !validName(name) || known[name] || !selectedName(names, name) {
					continue
				}
				if info, statErr := s.root.Lstat(path.Join(name, "SKILL.md")); statErr == nil && info.Mode().IsRegular() {
					findings = append(findings, harness.SkillFinding{Code: "installation_missing", Path: filepath.Join(s.config.Root, name), Message: fmt.Sprintf("Skill %s (source %s) has files but no installation registration in the active runtime database. Use host skill recovery; drafts are not automatically installed.", name, s.config.ID)})
				}
				if len(findings) >= 128 {
					break
				}
			}
			if readErr != nil || count > 4096 || len(findings) >= 128 {
				_ = dir.Close()
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return nil, readErr
				}
				break
			}
		}
		if len(findings) >= 128 {
			break
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings, nil
}

// VisibilityError diagnoses only host-authorized sources for this session.
// Other workspaces and explicitly excluded names are never searched.
func (r *Registry) VisibilityError(ctx context.Context, selection harness.SkillSelection, name string) error {
	if !validName(name) {
		return harness.ErrInvalidInput
	}
	missing := &harness.SkillAvailabilityError{Reason: "not_in_snapshot"}
	if !selectedName(selection.Names, name) {
		return missing
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	sources, err := r.selectedSources(selection)
	if err != nil {
		return err
	}
	for _, s := range sources {
		var enabled bool
		var status string
		err = r.db.QueryRowContext(ctx, `SELECT i.enabled,COALESCE(st.status,'unavailable') FROM df_skill_installs i LEFT JOIN df_skill_source_status st ON st.source_key=i.source_key AND st.name=i.name WHERE i.source_key=? AND i.name=?`, s.identity, name).Scan(&enabled, &status)
		if err == nil {
			if !enabled {
				return &harness.SkillAvailabilityError{Reason: "disabled"}
			}
			if status != "ready" {
				return &harness.SkillAvailabilityError{Reason: "source_unavailable"}
			}
			return missing
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	findings, err := r.unregisteredFindings(ctx, sources, []string{name})
	if err != nil {
		return err
	}
	for _, f := range findings {
		if f.Code == "installation_missing" {
			return &harness.SkillAvailabilityError{Reason: f.Code}
		}
	}
	return missing
}
