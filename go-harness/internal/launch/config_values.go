package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/internal/runtimepaths"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// ConfigValues resolves the value references of one configuration document:
// $NAME, ${NAME} and ${NAME:-fallback} from the environment, and
// ${secret:NAME} from the secret vault of the runtime data directory. The
// zero value has no vault values.
type ConfigValues struct {
	secrets     map[string]string
	binary      map[string]bool
	unavailable map[string]bool
	err         error // why the vault could not be read; reported on use
}

// ReadConfigValues reads the vault values document refers to, from the data
// directory that a runtime started with configPath and dataDir uses. It opens
// the database read-only, so a running runtime is unaffected, and reads
// nothing when document has no ${secret:NAME} reference. A failure surfaces
// only when a reference is resolved, so a reference in a setting the runtime
// ignores never stops it.
func ReadConfigValues(configPath, dataDir string, document any) ConfigValues {
	names := secretReferences(document, nil)
	if len(names) == 0 {
		return ConfigValues{}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	values := ConfigValues{secrets: map[string]string{}, binary: map[string]bool{}, unavailable: map[string]bool{}}
	found, unavailable, err := readConfigSecrets(configPath, dataDir, names)
	if err != nil {
		values.err = err
		return values
	}
	for _, name := range unavailable {
		values.unavailable[name] = true
	}
	for name, secret := range found {
		if utf8.Valid(secret.Value) && !bytes.ContainsRune(secret.Value, 0) {
			values.secrets[name] = string(secret.Value)
		} else {
			values.binary[name] = true
		}
		clear(secret.Value)
	}
	return values
}

func secretReferences(value any, names []string) []string {
	switch node := value.(type) {
	case string:
		if name, ok, err := secrets.Reference(node); ok && err == nil {
			names = append(names, name)
		}
	case map[string]any:
		for _, item := range node {
			names = secretReferences(item, names)
		}
	case []any:
		for _, item := range node {
			names = secretReferences(item, names)
		}
	}
	return names
}

func readConfigSecrets(configPath, dataDir string, names []string) (map[string]secrets.Secret, []string, error) {
	dir, err := runtimepaths.Resolve(configPath, dataDir)
	if err != nil {
		return nil, nil, err
	}
	if dir == "" {
		return nil, nil, errors.New("the runtime data directory is unknown")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, nil, err
	}
	// Open resolves the same aliases before it uses the directory.
	if dir, err = filepath.EvalSymlinks(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	db, err := sqlite.OpenReadOnly(runtimepaths.Database(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return secrets.ReadCurrentStatus(ctx, db, runtimepaths.SecretVault(dir), names)
}

// Resolve returns value, or the value its reference names.
func (c ConfigValues) Resolve(value string) (string, error) {
	name, ok, err := secrets.Reference(value)
	if err != nil {
		return "", err
	}
	if !ok {
		return resolveEnvironmentReference(value)
	}
	if found, exists := c.secrets[name]; exists {
		return found, nil
	}
	switch {
	case c.binary[name]:
		return "", fmt.Errorf("secret %s required by config is binary; a configuration value must be text", name)
	case c.unavailable[name]:
		return "", fmt.Errorf("secret %s required by config %w", name, secrets.ErrUnavailable)
	case c.err != nil:
		return "", fmt.Errorf("secret %s required by config cannot be read from the secret vault: %w", name, c.err)
	}
	return "", fmt.Errorf("secret %s required by config is missing from the secret vault", name)
}

func resolveConfigValue(value string) (string, error) { return ConfigValues{}.Resolve(value) }

func resolveEnvironmentReference(value string) (string, error) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := value[2 : len(value)-1]
		if key, fallback, ok := strings.Cut(name, ":-"); ok {
			if current := os.Getenv(key); current != "" {
				return current, nil
			}
			return fallback, nil
		}
		value = "$" + name
	}
	if strings.HasPrefix(value, "$") {
		name := strings.TrimPrefix(value, "$")
		if name == "" || strings.ContainsAny(name, "${} ") {
			return "", fmt.Errorf("invalid environment reference")
		}
		current, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s required by config is missing", name)
		}
		return current, nil
	}
	return value, nil
}
