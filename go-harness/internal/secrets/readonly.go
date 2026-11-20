package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Configuration values refer to a vault secret as ${secret:NAME}.
const referencePrefix = "${secret:"

// Reference reports whether a configuration value is a ${secret:NAME}
// reference and returns NAME. A value shaped like a reference that names no
// valid secret is an error, so it is never taken for a literal value.
func Reference(value string) (string, bool, error) {
	if !strings.HasPrefix(value, referencePrefix) || !strings.HasSuffix(value, "}") {
		return "", false, nil
	}
	name := value[len(referencePrefix) : len(value)-1]
	if !ValidName(name) {
		return "", true, fmt.Errorf("%w: invalid secret reference; write ${secret:NAME} with the secret's name in upper-case letters, digits and underscores", ErrInvalid)
	}
	return name, true, nil
}

// ReferenceTo returns the configuration value that refers to secret name.
func ReferenceTo(name string) string { return referencePrefix + name + "}" }

// ReadCurrent returns the current values of names from the vault kept in db
// and dir without changing either: it creates no key, table or directory and
// records no use. Names that are absent, deleted or encrypted under a vault
// key that is no longer available are left out. It serves configuration that
// is read before a runtime opens the vault, possibly while one is running.
func ReadCurrent(ctx context.Context, db *sql.DB, dir string, names []string) (map[string]Secret, error) {
	found, _, err := ReadCurrentStatus(ctx, db, dir, names)
	return found, err
}

// ReadCurrentStatus behaves like ReadCurrent and also reports names whose
// stored ciphertext was encrypted under a vault key that is no longer active.
func ReadCurrentStatus(ctx context.Context, db *sql.DB, dir string, names []string) (map[string]Secret, []string, error) {
	for _, name := range names {
		if err := validateName(name); err != nil {
			return nil, nil, err
		}
	}
	out := map[string]Secret{}
	if len(names) == 0 {
		return out, nil, nil
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := checkKeyFile(keyPath); errors.Is(err, fs.ErrNotExist) {
		return out, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	master, err := openKey(raw)
	if err != nil || len(master) != 32 {
		clear(master)
		return nil, nil, fmt.Errorf("%w: the secret vault key cannot be unlocked by this user", ErrUnavailable)
	}
	v := keyed(db, dir, master)
	defer func() {
		clear(v.encKey)
		clear(v.macKey)
		clear(v.sealKey)
	}()
	var tables int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('harness_secrets','harness_secret_versions')`).Scan(&tables); err != nil {
		return nil, nil, err
	}
	if tables < 2 {
		return out, nil, nil
	}
	var unavailable []string
	seenUnavailable := map[string]bool{}
	for _, name := range names {
		values, err := v.resolve(ctx, []string{name}, false)
		var missing *MissingError
		if errors.As(err, &missing) {
			continue
		}
		if errors.Is(err, ErrUnavailable) {
			if !seenUnavailable[name] {
				seenUnavailable[name] = true
				unavailable = append(unavailable, name)
			}
			continue
		}
		if err != nil {
			for _, secret := range out {
				clear(secret.Value)
			}
			return nil, nil, err
		}
		out[name] = values[name]
	}
	return out, unavailable, nil
}
