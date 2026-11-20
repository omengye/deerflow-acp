//go:build !windows

package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

var keyPrefix = []byte("DFSV-KEY-1\x00")

// Unix keeps the master key in a 0600 file inside the private 0700 vault
// directory. It protects database copies, not a compromised user account.
func sealKey(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty secret vault key")
	}
	return append(append([]byte(nil), keyPrefix...), raw...), nil
}

func openKey(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, keyPrefix) || len(raw) <= len(keyPrefix) {
		return nil, errors.New("unsupported secret vault key envelope")
	}
	return append([]byte(nil), raw[len(keyPrefix):]...), nil
}

func checkKeyFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("secret vault key is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("secret vault key permissions are too open and cannot be repaired: %w", err)
		}
	}
	return nil
}
