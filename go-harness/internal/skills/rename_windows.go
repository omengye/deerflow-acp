package skills

import (
	"errors"
	"fmt"
	"os"
	"path"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Root.Rename permits replacement. Journal backups and installs must never
// replace a racing user's target, so rename pinned handles without that flag.
func renameNoReplace(root *os.Root, from, to string) error {
	source, err := openExportDir(root, path.Dir(from), false)
	if err != nil {
		return err
	}
	defer source.Close()
	dest, err := openExportDir(root, path.Dir(to), false)
	if err != nil {
		return err
	}
	defer dest.Close()
	info, err := source.Lstat(path.Base(from))
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return denied("rename source is a link")
	}
	parent, err := source.Open(".")
	if err != nil {
		return err
	}
	defer parent.Close()
	nativeName, err := windows.NewNTUnicodeString(path.Base(from))
	if err != nil {
		return err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: nativeName}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return fmt.Errorf("open rename source: %w", err)
	}
	f := os.NewFile(uintptr(h), from)
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, actual) {
		return denied("rename source changed")
	}
	d, err := dest.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	name, err := windows.UTF16FromString(path.Base(to))
	if err != nil {
		return err
	}
	var header struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	}
	size := int(unsafe.Offsetof(header.Name)) + (len(name)-1)*2
	buf := make([]byte, size)
	p := (*struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	})(unsafe.Pointer(&buf[0]))
	p.Root, p.Length = windows.Handle(d.Fd()), uint32((len(name)-1)*2)
	copy(unsafe.Slice(&p.Name[0], len(name)-1), name)
	err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, &buf[0], uint32(len(buf)), windows.FileRenameInformation)
	var status windows.NTStatus
	if errors.As(err, &status) {
		return fmt.Errorf("exclusive rename: %w", status.Errno())
	}
	return err
}
