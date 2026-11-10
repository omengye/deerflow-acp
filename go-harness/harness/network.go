package harness

// NetworkConfig is host-owned connectivity policy. Never expose proxy credentials
// in model metadata or persisted session options.
type NetworkConfig struct {
	Proxy ProxyConfig `yaml:"proxy" json:"proxy"`
}

type ProxyConfig struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	URL     string   `yaml:"url" json:"url"`
	NoProxy []string `yaml:"no_proxy" json:"no_proxy"`
}
