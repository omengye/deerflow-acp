// Package websearch supplies host-owned search configuration and independent
// providers. It never asks a model to read credentials or construct HTTP calls.
package websearch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"go.yaml.in/yaml/v3"
)

var Names = []string{"brave", "deepseek", "searxng"}

func ValidProvider(name string) bool {
	return name == "brave" || name == "deepseek" || name == "searxng"
}

func Label(name string) string {
	switch name {
	case "brave":
		return "Brave"
	case "deepseek":
		return "DeepSeek"
	case "searxng":
		return "SearXNG"
	}
	return name
}

func Defaults() harness.SearchConfig {
	return harness.SearchConfig{DefaultProvider: "auto", FetchProvider: "local", Providers: map[string]harness.SearchProviderConfig{
		"brave":    {APIKey: "${BRAVE_API_KEY:-}"},
		"deepseek": {APIKey: "${DEEPSEEK_API_KEY:-}"},
		"searxng":  {BaseURL: "${SEARXNG_BASE_URL:-}", APIKey: "${SEARXNG_API_KEY:-${SEARXNG_TOKEN:-}}"},
	}}
}

// SeedYAML reuses legacy Brave settings and official DeepSeek model credentials.
// The same projection is used by first-install configuration and the daemon.
func SeedYAML(body []byte) (harness.SearchConfig, error) {
	var doc struct {
		Search yaml.Node   `yaml:"search"`
		Tools  []yaml.Node `yaml:"tools"`
		Models []struct {
			APIKey  string `yaml:"api_key"`
			BaseURL string `yaml:"base_url"`
		} `yaml:"models"`
	}
	if yaml.Unmarshal(body, &doc) != nil {
		return harness.SearchConfig{}, errors.New("invalid search configuration YAML")
	}
	var search harness.SearchConfig
	if doc.Search.Kind != 0 && doc.Search.Tag != "!!null" {
		body, err := yaml.Marshal(&doc.Search)
		if err != nil {
			return search, errors.New("invalid search configuration")
		}
		decoder := yaml.NewDecoder(bytes.NewReader(body))
		decoder.KnownFields(true)
		if err := decoder.Decode(&search); err != nil {
			return search, errors.New("invalid search configuration fields or types")
		}
	}
	var deepseekKey string
	for _, model := range doc.Models {
		u, err := url.Parse(resolve(model.BaseURL))
		if err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.deepseek.com") && (u.Port() == "" || u.Port() == "443") && u.User == nil && resolve(model.APIKey) != "" {
			deepseekKey = model.APIKey
			break
		}
	}
	var legacy []harness.BuiltinToolConfig
	for _, node := range doc.Tools {
		var identity struct {
			Name string `yaml:"name"`
		}
		if node.Decode(&identity) != nil || identity.Name != "web_search" {
			continue
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "timeout" && node.Content[i+1].Tag != "!!int" && node.Content[i+1].Tag != "!!null" {
				return search, errors.New("legacy web search timeout must be an integer")
			}
		}
		var fields struct {
			APIKey     string `yaml:"api_key"`
			HTTPSProxy string `yaml:"https_proxy"`
			Timeout    int    `yaml:"timeout"`
		}
		if node.Decode(&fields) != nil {
			return search, errors.New("invalid legacy web search configuration")
		}
		legacy = append(legacy, harness.BuiltinToolConfig{Name: "web_search", APIKey: fields.APIKey, HTTPSProxy: fields.HTTPSProxy, Timeout: fields.Timeout})
	}
	if deepseekKey == "" {
		// The former skill documented this user-owned credential file. Reading
		// its known schema avoids asking for the same credential a second time.
		if home, err := os.UserHomeDir(); err == nil {
			if body, err := readBounded(filepath.Join(home, ".config", "deepseek-search", "config.json")); err == nil {
				var local struct {
					APIKey string `json:"api_key"`
				}
				if json.Unmarshal(body, &local) == nil {
					deepseekKey = local.APIKey
				}
			}
		}
	}
	return Seed(search, legacy, deepseekKey), nil
}

func Seed(config harness.SearchConfig, tools []harness.BuiltinToolConfig, deepseekKey string) harness.SearchConfig {
	config = clone(config)
	defaults := Defaults()
	if config.Providers == nil {
		config.Providers = map[string]harness.SearchProviderConfig{}
	}
	for name, value := range defaults.Providers {
		if _, ok := config.Providers[name]; !ok {
			config.Providers[name] = value
		}
	}
	for _, tool := range tools {
		if tool.Name != "web_search" {
			continue
		}
		p := config.Providers["brave"]
		if resolve(p.APIKey) == "" && tool.APIKey != "" {
			p.APIKey = tool.APIKey
		}
		if p.HTTPSProxy == "" {
			p.HTTPSProxy = tool.HTTPSProxy
		}
		if p.Timeout == 0 {
			p.Timeout = tool.Timeout
		}
		config.Providers["brave"] = p
	}
	p := config.Providers["deepseek"]
	if resolve(p.APIKey) == "" && deepseekKey != "" {
		p.APIKey = deepseekKey
	}
	config.Providers["deepseek"] = p
	if config.DefaultProvider == "" {
		config.DefaultProvider = "auto"
	}
	if config.FetchProvider == "" {
		config.FetchProvider = "local"
	}
	return config
}

// resolve supports the existing $VAR / ${VAR:-fallback} convention, including
// the SearXNG API_KEY -> TOKEN compatibility fallback. Unset values stay empty.
// A ${secret:NAME} reference stays as written: a snapshot resolves it for each
// request, so settings and pinned versions never hold the secret's value.
func resolve(value string) string {
	for i := 0; i < 8 && strings.HasPrefix(value, "$"); i++ {
		if secretReference(value) {
			return value
		}
		if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
			inner := value[2 : len(value)-1]
			name, fallback, _ := strings.Cut(inner, ":-")
			if found := os.Getenv(name); found != "" {
				value = found
			} else {
				value = fallback
			}
		} else {
			value = os.Getenv(strings.TrimPrefix(value, "$"))
		}
	}
	return value
}

func resolved(name string, p harness.SearchProviderConfig) harness.SearchProviderConfig {
	p.APIKey, p.BaseURL, p.HTTPSProxy = resolve(p.APIKey), resolve(p.BaseURL), resolve(p.HTTPSProxy)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.BaseURL == "" {
		switch name {
		case "brave":
			p.BaseURL = "https://api.search.brave.com"
		case "deepseek":
			p.BaseURL = "https://api.deepseek.com"
		}
	}
	if p.Timeout == 0 {
		p.Timeout = 30
	}
	if p.AuthHeader == "" {
		p.AuthHeader = "Authorization"
	}
	return p
}

func Validate(config harness.SearchConfig) error {
	if config.DefaultProvider != "" && config.DefaultProvider != "auto" && !ValidProvider(config.DefaultProvider) {
		return errors.New("unknown default search provider")
	}
	if config.FetchProvider != "" && config.FetchProvider != "local" && config.FetchProvider != "deepseek" {
		return errors.New("fetch provider must be local or deepseek")
	}
	for name, raw := range config.Providers {
		if !ValidProvider(name) {
			return errors.New("unknown search provider")
		}
		p := resolved(name, raw)
		if p.Timeout < 1 || p.Timeout > 120 {
			return errors.New("search timeout must be 1..120 seconds")
		}
		if strings.ContainsAny(p.APIKey, "\r\n\x00") || len(p.APIKey) > 16384 {
			return errors.New("invalid search credential")
		}
		if _, ok, err := secrets.Reference(p.APIKey); ok && err != nil {
			return err
		}
		if secretReference(p.BaseURL) || secretReference(p.HTTPSProxy) {
			return errors.New("search base_url and https_proxy cannot refer to secrets; only api_key can")
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || !validURL(u) || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("search base URL must be HTTP(S), without credentials, query or fragment")
			}
		}
		if p.HTTPSProxy != "" {
			u, err := url.Parse(p.HTTPSProxy)
			if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
				return errors.New("invalid search proxy")
			}
		}
		if p.AuthHeader != "Authorization" && p.AuthHeader != "X-API-Key" {
			return errors.New("search auth_header must be Authorization or X-API-Key")
		}
	}
	return nil
}

func secretReference(value string) bool {
	_, ok, _ := secrets.Reference(value)
	return ok
}

func configured(name string, p harness.SearchProviderConfig) bool {
	if name == "searxng" {
		return p.BaseURL != ""
	}
	return p.APIKey != ""
}

func digest(value any) string {
	body, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func validURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
