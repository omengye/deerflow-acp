//go:build !windows

package skills

import "os"

func secureSnapshotFile(file *os.File) error { return file.Chmod(0600) }

func preserveEditSecurity(root *os.Root, name string, replacement *os.File) error {
	info, err := root.Stat(name)
	if err != nil {
		return err
	}
	return replacement.Chmod(info.Mode().Perm())
}
