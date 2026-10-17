package skills

import (
	"golang.org/x/sys/unix"
	"os"
	"path"
)

func renameNoReplace(root *os.Root, from, to string) error {
	a, err := openExportDir(root, path.Dir(from), false)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := openExportDir(root, path.Dir(to), false)
	if err != nil {
		return err
	}
	defer b.Close()
	af, err := a.Open(".")
	if err != nil {
		return err
	}
	defer af.Close()
	bf, err := b.Open(".")
	if err != nil {
		return err
	}
	defer bf.Close()
	return unix.Renameat2(int(af.Fd()), path.Base(from), int(bf.Fd()), path.Base(to), unix.RENAME_NOREPLACE)
}
