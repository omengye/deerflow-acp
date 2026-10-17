package wechat

import (
	"bytes"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var credentialPrefix = []byte("DFWX-DPAPI-1\x00")

func sealCredentials(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty credential payload")
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("cannot protect WeChat credentials for current Windows user")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append(append([]byte(nil), credentialPrefix...), unsafe.Slice(out.Data, out.Size)...), nil
}
func openCredentials(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, credentialPrefix) || len(raw) <= len(credentialPrefix) {
		return nil, errors.New("unsupported WeChat credential envelope; sign in again")
	}
	raw = raw[len(credentialPrefix):]
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("cannot unlock WeChat credentials for current Windows user; sign in again")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
