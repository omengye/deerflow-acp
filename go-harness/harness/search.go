package harness

import "fmt"

// ErrPinnedSearchConfigInvalid indicates that the search configuration bound
// to an execution cannot be reconstructed from its saved provider versions.
var ErrPinnedSearchConfigInvalid = fmt.Errorf("%w: pinned search configuration is invalid", ErrPinnedExecutionResourcesInvalid)

// SearchConfig belongs to the host. Credentials must never be projected into
// model metadata or durable execution input; SearchResourceRef pins only hashes.
type SearchConfig struct {
	DefaultProvider string                          `yaml:"default_provider,omitempty" json:"default_provider,omitempty"`
	FetchProvider   string                          `yaml:"fetch_provider,omitempty" json:"fetch_provider,omitempty"`
	Providers       map[string]SearchProviderConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
}

type SearchProviderConfig struct {
	Network    NetworkConfig `yaml:"-" json:"-"`
	BaseURL    string        `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKey     string        `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	HTTPSProxy string        `yaml:"https_proxy,omitempty" json:"https_proxy,omitempty"`
	AuthHeader string        `yaml:"auth_header,omitempty" json:"auth_header,omitempty"`
	Timeout    int           `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type SearchResourceRef struct {
	DefaultProvider string            `json:"defaultProvider"`
	FetchProvider   string            `json:"fetchProvider"`
	Providers       map[string]string `json:"providers"`
}
