package desktopconfig

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
)

// Old portable profiles have no provenance manifest. Detect drift but never
// mistake a locally edited builtin for an obsolete file safe to overwrite.
func bundledSkillDiagnostics(source, installed string) []string {
	entries, err := os.ReadDir(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []string{"Cannot inspect bundled skill versions; local skills were preserved."}
	}
	var warnings []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(warnings) >= 128 {
			break
		}
		want, e1 := skillTreeHashes(filepath.Join(source, entry.Name()))
		got, e2 := skillTreeHashes(filepath.Join(installed, entry.Name()))
		if e1 != nil || e2 != nil || !reflect.DeepEqual(want, got) {
			warnings = append(warnings, fmt.Sprintf("Bundled skill %s differs from the running package or is unavailable. Local files were preserved; review the complete package diff before updating %s.", entry.Name(), filepath.Join(installed, entry.Name())))
		}
	}
	return warnings
}

func skillTreeHashes(directory string) (map[string][32]byte, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	out := map[string][32]byte{}
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > 1024 || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsupported skill tree")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return fmt.Errorf("unsupported skill file")
		}
		body, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		out[name] = sha256.Sum256(body)
		return nil
	})
	return out, err
}
