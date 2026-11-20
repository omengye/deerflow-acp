package deerflow

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/sys/windows"
)

// Open relative to the pinned workspace parent. A write-sharing conflict
// fails before import. Disposition is applied to the handle we actually read.
func claimImportSource(parent *os.Root, base string) (*importSource, error) {
	defer parent.Close()
	dir, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name, err := windows.NewNTUnicodeString(base)
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_READ|windows.DELETE, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0, windows.FILE_SHARE_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: source missing, busy, or cannot be deleted", harness.ErrInvalidInput)
	}
	f := os.NewFile(uintptr(h), base)
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: delete_source requires an ordinary file", harness.ErrInvalidInput)
	}
	return &importSource{f, base, func(commit bool) error {
		var err error
		if commit {
			flag := byte(1)
			err = windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &flag, 1)
		}
		return errors.Join(err, f.Close())
	}}, nil
}
