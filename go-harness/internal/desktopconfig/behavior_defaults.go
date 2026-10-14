package desktopconfig

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const defaultBehaviorSuitePath = "./skill-behavior/searxng-web-search.json"

// Compiled host assessments cannot be supplied by a candidate or import source.
//
//go:embed defaults/searxng-web-search.json
var defaultBehaviorSuite []byte

func (s service) initializeDefaultBehaviorSuite(data map[string]any) error {
	behavior := readObject(readObject(data, "skill_evolution"), "behavior")
	if !boolValue(behavior, "enabled", false) || str(behavior, "suite_path", "") != defaultBehaviorSuitePath {
		return nil
	}
	configDir := filepath.Dir(s.config)
	if filepath.Clean(configDir) != filepath.Join(s.userData, "config") {
		return nil // External configuration directories remain operator-owned.
	}
	directory := filepath.Join(configDir, "skill-behavior")
	if err := ensurePortableDirectory(s.userData, directory); err != nil {
		return fmt.Errorf("initialize bundled behavior directory: %w", err)
	}
	expected, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return errors.New("bundled behavior directory changed while opening")
	}
	const name = "searxng-web-search.json"
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("bundled behavior path is not a regular file")
		}
		return nil // Never overwrite an existing host's assessments, even if invalid.
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(defaultBehaviorSuite)
	syncErr := file.Sync()
	err = errors.Join(writeErr, syncErr, file.Close())
	if err != nil {
		// Only remove the file created by this attempt; never an existing suite.
		return errors.Join(err, root.Remove(name))
	}
	return nil
}
