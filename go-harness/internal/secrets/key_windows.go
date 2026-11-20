package secrets

import (
	"bytes"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var keyPrefix = []byte("DFSV-DPAPI-1\x00")

// The master key is bound to the current Windows user by DPAPI, matching
// channel credentials. Another user or machine cannot unlock it.
func sealKey(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty secret vault key")
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("cannot protect the secret vault key for the current Windows user")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append(append([]byte(nil), keyPrefix...), unsafe.Slice(out.Data, out.Size)...), nil
}

func openKey(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, keyPrefix) || len(raw) <= len(keyPrefix) {
		return nil, errors.New("unsupported secret vault key envelope")
	}
	raw = raw[len(keyPrefix):]
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("cannot unlock the secret vault key for the current Windows user")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// DPAPI already binds the key to the user; NTFS ACLs of the profile protect
// the data directory. Reject symlinks and non-regular files consistently.
func checkKeyFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("secret vault key is not a regular file")
	}
	return nil
}
