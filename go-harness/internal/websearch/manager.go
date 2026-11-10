package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
)

// Manager keeps mutable preferences separate from immutable execution versions.
// Selection changes do not modify YAML or invalidate existing run checkpoints.
type Manager struct {
	network harness.NetworkConfig
	path    string
	seed    harness.SearchConfig
	mu      sync.Mutex
	memory  *settings
	waiters map[string]int
}

type settings struct {
	Version  int                  `json:"version"`
	Config   harness.SearchConfig `json:"config"`
	Verified map[string]string    `json:"verified,omitempty"`
}

type ProviderStatus struct {
	HTTPSProxyConfigured bool     `json:"https_proxy_configured"`
	Name                 string   `json:"name"`
	Label                string   `json:"label"`
	BaseURL              string   `json:"base_url"`
	Configured           bool     `json:"configured"`
	APIKeyConfigured     bool     `json:"api_key_configured"`
	State                string   `json:"state"`
	Missing              []string `json:"missing_fields"`
	AuthHeader           string   `json:"auth_header"`
	Timeout              int      `json:"timeout"`
}

type Status struct {
	DefaultProvider       string           `json:"default_provider"`
	FetchProvider         string           `json:"fetch_provider"`
	Revision              string           `json:"revision"`
	Providers             []ProviderStatus `json:"providers"`
	AwaitingConfiguration []string         `json:"awaiting_configuration,omitempty"`
}

type Update struct {
	Provider         string  `json:"provider"`
	ExpectedRevision string  `json:"expected_revision"`
	BaseURL          *string `json:"base_url,omitempty"`
	APIKey           *string `json:"api_key,omitempty"`
	ClearAPIKey      bool    `json:"clear_api_key,omitempty"`
	HTTPSProxy       *string `json:"https_proxy,omitempty"`
	AuthHeader       *string `json:"auth_header,omitempty"`
	Timeout          *int    `json:"timeout,omitempty"`
	MakeDefault      bool    `json:"make_default,omitempty"`
	FetchProvider    string  `json:"fetch_provider,omitempty"`
}

func New(path string, seed harness.SearchConfig, policies ...harness.NetworkConfig) (*Manager, error) {
	var policy harness.NetworkConfig
	if len(policies) > 0 {
		policy = network.Clone(policies[0])
	}
	if err := network.Validate(policy); err != nil {
		return nil, err
	}
	seed = Seed(seed, nil, "")
	if err := Validate(seed); err != nil {
		return nil, err
	}
	return &Manager{path: path, seed: clone(seed), network: policy}, nil
}

func clone(config harness.SearchConfig) harness.SearchConfig {
	body, _ := json.Marshal(config)
	var result harness.SearchConfig
	_ = json.Unmarshal(body, &result)
	return result
}

func (m *Manager) load() (settings, error) {
	if m.path == "" {
		if m.memory != nil {
			value := *m.memory
			value.Config = clone(value.Config)
			return value, nil
		}
		return settings{Version: 1, Config: clone(m.seed)}, nil
	}
	body, err := readBounded(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return settings{Version: 1, Config: clone(m.seed)}, nil
	}
	if err != nil {
		return settings{}, errors.New("cannot read search settings")
	}
	var value settings
	if json.Unmarshal(body, &value) != nil || value.Version != 1 {
		return settings{}, errors.New("invalid search settings")
	}
	value.Config = Seed(value.Config, nil, "")
	for name, p := range value.Config.Providers {
		// Unresolved automatic references may now have a reusable official model
		// credential. Explicitly stored or cleared credentials remain authoritative.
		fallback := m.seed.Providers[name]
		if strings.HasPrefix(p.APIKey, "$") {
			if resolve(p.APIKey) == "" && resolve(fallback.APIKey) != "" {
				p.APIKey = fallback.APIKey
			}
			if p.HTTPSProxy == "" {
				p.HTTPSProxy = fallback.HTTPSProxy
			}
			if p.Timeout == 0 {
				p.Timeout = fallback.Timeout
			}
			value.Config.Providers[name] = p
		}
	}
	if err := Validate(value.Config); err != nil {
		return settings{}, err
	}
	return value, nil
}

func (m *Manager) save(value settings) error {
	if m.path == "" {
		m.memory = &value
		return nil
	}
	return writeAtomic(m.path, value)
}

// Initialize durably records the seed and any reused credentials before the
// desktop removes obsolete YAML fields. Existing selections and explicit clears
// remain authoritative. Repeated initialization does not rewrite unchanged data.
func (m *Manager) Initialize(ctx context.Context) error {
	_, err := m.mutate(ctx, func(*settings) error { return nil })
	return err
}

func defaultProvider(config harness.SearchConfig) string {
	if ValidProvider(config.DefaultProvider) {
		return config.DefaultProvider
	}
	for _, name := range Names {
		if configured(name, resolved(name, config.Providers[name])) {
			return name
		}
	}
	return "brave"
}

func status(value settings) Status {
	out := Status{DefaultProvider: defaultProvider(value.Config), FetchProvider: value.Config.FetchProvider, Revision: digest(value.Config), Providers: []ProviderStatus{}}
	for _, name := range Names {
		p := resolved(name, value.Config.Providers[name])
		entry := ProviderStatus{HTTPSProxyConfigured: p.HTTPSProxy != "", Name: name, Label: Label(name), BaseURL: p.BaseURL, APIKeyConfigured: p.APIKey != "", Configured: configured(name, p), State: "not_configured", Missing: []string{}, AuthHeader: p.AuthHeader, Timeout: p.Timeout}
		if entry.Configured {
			entry.State = "configured"
			if value.Verified[name] == digest(p) {
				entry.State = "verified"
			}
		} else if name == "searxng" {
			entry.Missing = append(entry.Missing, "base_url")
		} else {
			entry.Missing = append(entry.Missing, "api_key")
		}
		out.Providers = append(out.Providers, entry)
	}
	return out
}

func (m *Manager) Status() (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, err := m.load()
	if err != nil {
		return Status{}, err
	}
	out := status(value)
	for _, name := range Names {
		if m.waiters[name] > 0 {
			out.AwaitingConfiguration = append(out.AwaitingConfiguration, name)
		}
	}
	return out, nil
}

// AwaitConfiguration leaves the search call pending while the desktop displays
// its settings form. Saving configuration resumes that same query. A caller's
// cancellation ends the wait; an already configured execution never takes this
// path and keeps its pinned provider configuration.
func (m *Manager) AwaitConfiguration(ctx context.Context, name string) (harness.SearchProviderConfig, error) {
	m.mu.Lock()
	if m.waiters == nil {
		m.waiters = map[string]int{}
	}
	m.waiters[name]++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.waiters[name]--; m.mu.Unlock() }()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		value, err := m.load()
		m.mu.Unlock()
		if err != nil {
			return harness.SearchProviderConfig{}, err
		}
		p := resolved(name, value.Config.Providers[name])
		if configured(name, p) {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return harness.SearchProviderConfig{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) mutate(ctx context.Context, change func(*settings) error) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path != "" {
		if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
			return Status{}, errors.New("cannot create search settings directory")
		}
		lock := flock.New(m.path + ".lock")
		locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
		if err != nil || !locked {
			return Status{}, errors.New("search settings are busy; retry")
		}
		defer lock.Unlock()
	}
	value, err := m.load()
	if err != nil {
		return Status{}, err
	}
	if err := change(&value); err != nil {
		return Status{}, err
	}
	if err := Validate(value.Config); err != nil {
		return Status{}, err
	}
	if err := m.save(value); err != nil {
		return Status{}, errors.New("cannot save search settings")
	}
	return status(value), nil
}

func (m *Manager) Select(ctx context.Context, name string) (Status, error) {
	if !ValidProvider(name) {
		return Status{}, errors.New("unknown search provider")
	}
	return m.mutate(ctx, func(value *settings) error {
		if !configured(name, resolved(name, value.Config.Providers[name])) {
			return &ConfigurationError{Provider: name}
		}
		value.Config.DefaultProvider = name
		return nil
	})
}

func (m *Manager) Update(ctx context.Context, request Update) (Status, error) {
	if !ValidProvider(request.Provider) {
		return Status{}, errors.New("unknown search provider")
	}
	return m.mutate(ctx, func(value *settings) error {
		if request.ExpectedRevision == "" || request.ExpectedRevision != digest(value.Config) {
			return errors.New("search configuration changed; refresh and retry")
		}
		p := value.Config.Providers[request.Provider]
		if request.BaseURL != nil {
			p.BaseURL = strings.TrimSpace(*request.BaseURL)
		}
		// An empty masked form retains an existing key. Clearing is explicit.
		if request.APIKey != nil && *request.APIKey != "" {
			p.APIKey = strings.TrimSpace(*request.APIKey)
		}
		if request.ClearAPIKey {
			p.APIKey = ""
		}
		if request.HTTPSProxy != nil {
			p.HTTPSProxy = *request.HTTPSProxy
		}
		if request.AuthHeader != nil {
			p.AuthHeader = *request.AuthHeader
		}
		if request.Timeout != nil {
			p.Timeout = *request.Timeout
		}
		value.Config.Providers[request.Provider] = p
		if request.FetchProvider != "" {
			value.Config.FetchProvider = request.FetchProvider
		}
		if request.MakeDefault {
			if !configured(request.Provider, resolved(request.Provider, p)) {
				return errors.New("complete the required search configuration")
			}
			value.Config.DefaultProvider = request.Provider
		}
		return nil
	})
}

type Snapshot struct {
	network   harness.NetworkConfig
	Ref       harness.SearchResourceRef
	providers map[string]harness.SearchProviderConfig
	manager   *Manager
}

func (m *Manager) versionPath(hash string) string {
	return filepath.Join(m.path+".versions", hash+".json")
}

func (m *Manager) Snapshot(pinned *harness.SearchResourceRef) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := &Snapshot{providers: map[string]harness.SearchProviderConfig{}, manager: m, network: network.Clone(m.network)}
	if pinned != nil {
		if !ValidProvider(pinned.DefaultProvider) || pinned.FetchProvider != "local" && pinned.FetchProvider != "deepseek" || len(pinned.Providers) != len(Names) {
			return nil, errors.New("invalid search execution resources")
		}
		result.Ref = *pinned
		for _, name := range Names {
			hash := pinned.Providers[name]
			if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
				return nil, errors.New("invalid search resource digest")
			}
			if m.path == "" {
				value, err := m.load()
				if err != nil {
					return nil, err
				}
				p := resolved(name, value.Config.Providers[name])
				if digest(p) != hash {
					return nil, errors.New("search execution configuration changed")
				}
				result.providers[name] = p
				continue
			}
			body, err := readBounded(m.versionPath(hash))
			if err != nil {
				return nil, errors.New("pinned search configuration is unavailable")
			}
			var p harness.SearchProviderConfig
			if json.Unmarshal(body, &p) != nil || digest(p) != hash {
				return nil, errors.New("pinned search configuration is invalid")
			}
			result.providers[name] = p
		}
		return result, nil
	}
	value, err := m.load()
	if err != nil {
		return nil, err
	}
	result.Ref = harness.SearchResourceRef{DefaultProvider: defaultProvider(value.Config), FetchProvider: value.Config.FetchProvider, Providers: map[string]string{}}
	for _, name := range Names {
		p := resolved(name, value.Config.Providers[name])
		hash := digest(p)
		result.Ref.Providers[name] = hash
		result.providers[name] = p
		if m.path != "" {
			path := m.versionPath(hash)
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := writeAtomic(path, p); err != nil {
					return nil, errors.New("cannot pin search configuration")
				}
			} else if err != nil {
				return nil, errors.New("cannot inspect search configuration version")
			}
		}
	}
	return result, nil
}

func (s *Snapshot) verified(ctx context.Context, name string) {
	if s.manager == nil {
		return
	}
	_, _ = s.manager.mutate(ctx, func(value *settings) error {
		if value.Verified == nil {
			value.Verified = map[string]string{}
		}
		value.Verified[name] = digest(s.providers[name])
		return nil
	})
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if len(body) > 1<<20 {
		return nil, errors.New("search settings exceed size limit")
	}
	return body, err
}

func writeAtomic(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if current, err := readBounded(path); err == nil && bytes.Equal(current, body) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".search-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
