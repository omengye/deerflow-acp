package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

// storedSettings reads the settings file itself, without the seed: values
// that only come from the seed are written in config.yaml.
func (m *Manager) storedSettings() (settings, bool, error) {
	if m.path == "" {
		return settings{}, false, nil
	}
	body, err := readBounded(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return settings{}, false, nil
	}
	if err != nil {
		return settings{}, false, errors.New("cannot read search settings")
	}
	var value settings
	if json.Unmarshal(body, &value) != nil || value.Version != 1 {
		return settings{}, false, errors.New("invalid search settings")
	}
	return value, true, nil
}

// StoredPlaintextKeys returns, by provider, the api keys the settings file
// holds as plain text.
func (m *Manager) StoredPlaintextKeys() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok, err := m.storedSettings()
	if !ok || err != nil {
		return nil, err
	}
	keys := map[string]string{}
	for _, name := range Names {
		key := value.Config.Providers[name].APIKey
		if text := strings.TrimSpace(key); text != "" && !strings.HasPrefix(text, "$") {
			keys[name] = key
		}
	}
	return keys, nil
}

// SecretReferences returns, by secret name, the providers whose saved api_key
// refers to it.
func (m *Manager) SecretReferences() (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok, err := m.storedSettings()
	if !ok || err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, name := range Names {
		if secret, ok, err := secrets.Reference(value.Config.Providers[name].APIKey); ok && err == nil {
			out[secret] = append(out[secret], name)
		}
	}
	return out, nil
}

// KeyReference replaces a saved plaintext api key with a vault reference.
type KeyReference struct {
	Value  string // the plaintext the setting must still hold
	Secret string // the secret that now holds it
}

// errUnchanged ends a mutation that has nothing to save.
var errUnchanged = errors.New("search settings unchanged")

// ReferenceKeys replaces saved plaintext api keys with ${secret:NAME}
// references and returns the providers it changed. Only keys the settings
// file holds are replaced, and a key that changed since it was read is left
// alone. A provider verified with the plaintext stays verified, since the
// reference stands for the same value.
func (m *Manager) ReferenceKeys(ctx context.Context, keys map[string]KeyReference) ([]string, error) {
	for _, key := range keys {
		if !secrets.ValidName(key.Secret) {
			return nil, errors.New("invalid secret name for a search api_key")
		}
	}
	if m.path == "" || len(keys) == 0 {
		return nil, nil
	}
	var changed []string
	_, err := m.mutate(ctx, func(value *settings) error {
		changed = nil
		// Seeded values belong to config.yaml; saving them here would copy them.
		stored, ok, err := m.storedSettings()
		if err != nil {
			return err
		}
		if !ok {
			return errUnchanged
		}
		for _, name := range Names {
			key, ok := keys[name]
			p := value.Config.Providers[name]
			if !ok || p.APIKey != key.Value || stored.Config.Providers[name].APIKey != key.Value {
				continue
			}
			verified := value.Verified[name] != "" && value.Verified[name] == digest(resolved(name, p))
			p.APIKey = secrets.ReferenceTo(key.Secret)
			value.Config.Providers[name] = p
			if verified {
				value.Verified[name] = digest(resolved(name, p))
			}
			changed = append(changed, name)
		}
		if len(changed) == 0 {
			return errUnchanged
		}
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// PublishMigration synchronizes the live seed, the settings file and the YAML
// publisher under the same lock as snapshot verification. Persisted references
// are usable as soon as the vault commits, even if YAML publication must retry.
func (m *Manager) PublishMigration(ctx context.Context, keys map[string]KeyReference, yaml []byte, imported map[string]string, publish func() error) ([]string, error) {
	var seed harness.SearchConfig
	var err error
	if len(yaml) > 0 {
		if seed, err = SeedYAML(yaml); err != nil {
			return nil, err
		}
	}
	var changed []string
	err = m.withSettingsLock(ctx, func() error {
		value, err := m.load()
		if err != nil {
			return err
		}
		stored, hasStored, err := m.storedSettings()
		if err != nil {
			return err
		}
		nextSeed := clone(m.seed)
		dirty := false
		for _, name := range Names {
			p := value.Config.Providers[name]
			old := m.seed.Providers[name]
			next := seed.Providers[name].APIKey
			ref, _, _ := secrets.Reference(next)
			if ref != "" && imported[old.APIKey] == ref {
				oldValue := old.APIKey
				old.APIKey = next
				nextSeed.Providers[name] = old
				if p.APIKey == oldValue {
					p.APIKey = next
				}
			}
			key, ok := keys[name]
			if ok && hasStored && stored.Config.Providers[name].APIKey == key.Value && value.Config.Providers[name].APIKey == key.Value {
				p.APIKey = secrets.ReferenceTo(key.Secret)
				changed = append(changed, name)
			}
			before := value.Config.Providers[name]
			if p.APIKey != before.APIKey {
				if value.Verified[name] == digest(resolved(name, before)) {
					value.Verified[name] = digest(resolved(name, p))
				}
				value.Config.Providers[name] = p
				dirty = true
			}
		}
		if dirty {
			if err = Validate(value.Config); err != nil {
				return err
			}
			if err = m.save(value); err != nil {
				return err
			}
		}
		if err = publish(); err != nil {
			return err
		}
		m.seed = nextSeed
		return nil
	})
	return changed, err
}

// PinnedPlaintextVersions counts the pinned execution versions that hold an
// api key as plain text. They are never rewritten: a run pinned to one
// resumes with exactly the configuration it started with.
func (m *Manager) PinnedPlaintextVersions() (int, error) {
	if m.path == "" {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := m.path + ".versions"
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := readBounded(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var p harness.SearchProviderConfig
		if json.Unmarshal(body, &p) == nil && strings.TrimSpace(p.APIKey) != "" && !secretReference(p.APIKey) {
			count++
		}
	}
	return count, nil
}
