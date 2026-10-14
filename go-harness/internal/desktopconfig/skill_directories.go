package desktopconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Only stores owned by this portable profile may be provisioned automatically.
// Custom external roots remain explicit, validated settings. Failed provisioning
// is reported by snapshot without taking away the user's configuration editor.
func (s service) initializePortableSkillDirectories(data map[string]any) []string {
	settings := readObject(data, "skills")
	var diagnostics []string
	create := func(field, value, owned string) {
		if value == "" {
			return
		}
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(s.config), path)
		}
		owned = filepath.Join(s.userData, owned)
		rel, err := filepath.Rel(owned, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return
		}
		if err := ensurePortableDirectory(s.userData, path); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("%s: cannot initialize portable directory: %v", field, err))
		}
	}
	// Older profiles have only skills.path, without managed_sources. Provision
	// their writable source even before evolution is enabled in Settings.
	// This runs after bundled skills are copied so an empty custom directory
	// cannot cause copyInitialSkills to skip the initial import.
	create("skills.path/custom", filepath.Join(s.paths(data).skills, "custom"), "skills")
	for i, item := range list(settings, "managed_sources") {
		if source, ok := item.(map[string]any); ok {
			create(fmt.Sprintf("skills.managed_sources.%d.root", i), str(source, "root", ""), "skills")
		}
	}
	create("skills.export_root", str(settings, "export_root", ""), "skills-exports")
	return diagnostics
}

func ensurePortableDirectory(userData, path string) error {
	// Registry policy rejects links in all root components. Apply the same
	// rule before any creation, then keep all writes inside an opened os.Root.
	for parent := filepath.Clean(userData); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("profile contains a link or a non-directory component")
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	expected, err := os.Lstat(userData)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(userData)
	if err != nil {
		return err
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return errors.New("profile changed while opening")
	}
	rel, err := filepath.Rel(userData, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("directory is outside the portable profile")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		info, err := current.Lstat(part)
		if errors.Is(err, os.ErrNotExist) {
			if err = current.Mkdir(part, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = current.Lstat(part)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("directory contains a link or a non-directory component")
		}
		// Pin each checked component instead of traversing a checked prefix
		// again: another process cannot replace that prefix with a link and
		// redirect a later creation to a different directory.
		next, err := current.OpenRoot(part)
		if err != nil {
			return err
		}
		defer next.Close()
		actual, err := next.Stat(".")
		if err != nil || !os.SameFile(info, actual) {
			return errors.New("directory changed while opening")
		}
		current = next
	}
	return nil
}
