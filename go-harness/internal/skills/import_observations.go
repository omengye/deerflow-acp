package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxImportChanges = 100

type importOrigin struct {
	name, version, directory string
}

// ObserveImports checks only previously imported managed packages, including
// deleted publications and directories no longer in the startup Install list.
// Filesystem failures become sanitized advisory observations. Cancellation and
// database/integrity failures remain errors. No observed content is installed.
func (r *Registry) ObserveImports(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	for _, s := range r.sources {
		rows, err := r.db.QueryContext(ctx, `SELECT name,last_import_version,import_directory FROM df_skill_control WHERE source_key=? AND managed=1 AND last_import_version<>'' AND import_directory<>'' ORDER BY name`, s.identity)
		if err != nil {
			return err
		}
		var origins []importOrigin
		for rows.Next() {
			var origin importOrigin
			if err = rows.Scan(&origin.name, &origin.version, &origin.directory); err != nil {
				break
			}
			origins = append(origins, origin)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close() // The shared database may have only one connection.
		if err != nil {
			return err
		}
		for _, origin := range origins {
			if err = r.observeImport(ctx, s, origin); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (r *Registry) observeImport(ctx context.Context, s *source, origin importOrigin) error {
	if !validName(origin.name) || !validHash(origin.version) || origin.directory != "." && !validRelative(origin.directory) {
		return errors.New("skill import origin is inconsistent")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	observation := harness.SkillImportObservation{
		Target:          harness.SkillTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Name: origin.name, Scope: s.config.Scope, Workspace: s.config.Workspace},
		ImportedVersion: origin.version, CheckedAt: time.Now().UTC(),
	}
	// Check each component without following links. Missing directories or a
	// missing SKILL.md are distinguishable from malformed/inaccessible content.
	observation.Status = importAvailability(s, origin.directory)
	var observed skillPackage
	if observation.Status == "" {
		var err error
		observed, err = r.readPackage(ctx, s, origin.directory)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, harness.ErrInvalidInput):
			observation.Status = "invalid"
		case err != nil:
			observation.Status = "unavailable"
		case observed.meta.Name != origin.name:
			observation.Status = "invalid"
		default:
			observation.Status = "unchanged"
			observation.ObservedVersion = (&Candidate{pkg: observed}).Version()
			if observation.ObservedVersion != origin.version {
				observation.Status = "changed"
			}
		}
	}
	// Disk reads happen outside the SQL transaction. Recheck the import identity
	// before storing a report, so a concurrent explicit import cannot be confused
	// with the baseline that was actually observed.
	return r.withPublication(ctx, s, origin.name, func(tx *PublicationTx) error {
		var matches bool
		if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_control WHERE source_key=? AND name=? AND managed=1 AND last_import_version=? AND import_directory=?)`, s.identity, origin.name, origin.version, origin.directory).Scan(&matches); err != nil || !matches {
			return err
		}
		base, err := tx.ReadVersion(origin.version)
		if err != nil {
			return err
		}
		if observation.Status == "changed" {
			observation.Changes, observation.Truncated = importChanges(base.pkg.manifest, observed.manifest)
		}
		body, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_import_observations(source_key,name,import_version,import_directory,checked_at_ns,payload) VALUES(?,?,?,?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET import_version=excluded.import_version,import_directory=excluded.import_directory,checked_at_ns=excluded.checked_at_ns,payload=excluded.payload WHERE df_skill_import_observations.checked_at_ns<=excluded.checked_at_ns`, s.identity, origin.name, origin.version, origin.directory, observation.CheckedAt.UnixNano(), string(body))
		return err
	})
}

func importAvailability(s *source, directory string) string {
	var parts []string
	for prefix := directory; prefix != "."; prefix = path.Dir(prefix) {
		parts = append(parts, prefix)
	}
	parts = append(parts, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		info, err := s.root.Lstat(parts[i])
		if errors.Is(err, os.ErrNotExist) {
			return "missing"
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "unavailable"
		}
		if !info.IsDir() {
			return "invalid"
		}
	}
	info, err := s.root.Lstat(path.Join(directory, "SKILL.md"))
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "unavailable"
	}
	return ""
}

func importChanges(before, after []harness.SkillFileInfo) ([]harness.SkillImportFileChange, bool) {
	changes := make(map[string]harness.SkillImportFileChange, len(before)+len(after))
	for _, file := range before {
		changes[file.Path] = harness.SkillImportFileChange{Path: file.Path, Change: "removed", BeforeHash: file.Hash}
	}
	for _, file := range after {
		change, exists := changes[file.Path]
		if exists && change.BeforeHash == file.Hash {
			delete(changes, file.Path)
			continue
		}
		change.Path, change.AfterHash = file.Path, file.Hash
		change.Change = "added"
		if exists {
			change.Change = "modified"
		}
		changes[file.Path] = change
	}
	paths := make([]string, 0, len(changes))
	for name := range changes {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	truncated := len(paths) > maxImportChanges
	if truncated {
		paths = paths[:maxImportChanges]
	}
	result := make([]harness.SkillImportFileChange, 0, len(paths))
	for _, name := range paths {
		result = append(result, changes[name])
	}
	return result, truncated
}

// ListImportObservations reads saved advisory data without touching source
// files. Source selection follows the same operator scope rules as List.
func (r *Registry) ListImportObservations(ctx context.Context, selection harness.SkillSelection) ([]harness.SkillImportObservation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	sources, err := r.selectedSources(selection)
	if err != nil {
		return nil, err
	}
	result := make([]harness.SkillImportObservation, 0)
	for _, s := range sources {
		rows, err := r.db.QueryContext(ctx, `SELECT o.name,o.import_version,o.checked_at_ns,o.payload FROM df_skill_import_observations o JOIN df_skill_control c ON c.source_key=o.source_key AND c.name=o.name AND c.last_import_version=o.import_version AND c.import_directory=o.import_directory WHERE o.source_key=? AND c.managed=1 ORDER BY o.name`, s.identity)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, version, payload string
			var checkedAt int64
			if err = rows.Scan(&name, &version, &checkedAt, &payload); err != nil {
				break
			}
			if !selectedName(selection.Names, name) {
				continue
			}
			target := harness.SkillTarget{SourceID: s.config.ID, SourceIdentity: s.identity, Name: name, Scope: s.config.Scope, Workspace: s.config.Workspace}
			var o *harness.SkillImportObservation
			if o, err = decodeImportObservation(payload, target, version, checkedAt); err != nil {
				break
			}
			result = append(result, *o)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func decodeImportObservation(payload string, target harness.SkillTarget, version string, checkedAt int64) (*harness.SkillImportObservation, error) {
	var o harness.SkillImportObservation
	if json.Unmarshal([]byte(payload), &o) != nil || o.Target != target || o.ImportedVersion != version || !validHash(version) || o.CheckedAt.UnixNano() != checkedAt || !validImportObservation(o) {
		return nil, errors.New("skill import observation is inconsistent")
	}
	return &o, nil
}

func validImportObservation(o harness.SkillImportObservation) bool {
	switch o.Status {
	case "unchanged":
		return o.ObservedVersion == o.ImportedVersion && len(o.Changes) == 0 && !o.Truncated
	case "missing", "invalid", "unavailable":
		return o.ObservedVersion == "" && len(o.Changes) == 0 && !o.Truncated
	case "changed":
		if !validHash(o.ObservedVersion) || o.ObservedVersion == o.ImportedVersion || len(o.Changes) == 0 || len(o.Changes) > maxImportChanges || o.Truncated && len(o.Changes) != maxImportChanges {
			return false
		}
		previous := ""
		for _, change := range o.Changes {
			if !validRelative(change.Path) || change.Path <= previous {
				return false
			}
			previous = change.Path
			switch change.Change {
			case "added":
				if change.BeforeHash != "" || !validHash(change.AfterHash) {
					return false
				}
			case "removed":
				if !validHash(change.BeforeHash) || change.AfterHash != "" {
					return false
				}
			case "modified":
				if !validHash(change.BeforeHash) || !validHash(change.AfterHash) || change.BeforeHash == change.AfterHash {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return false
}
