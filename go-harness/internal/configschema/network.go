package configschema

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
)

func networkFields() []Field {
	return []Field{
		{Path: "/network/proxy/enabled", Type: "boolean", Default: false},
		{Path: "/network/proxy/url", Type: "string", Default: "", MaxBytes: 8192},
		{Path: "/network/proxy/clear_url", Type: "boolean", Default: false},
		{Path: "/network/proxy/no_proxy", Type: "string_array", Default: []string{"localhost", "127.0.0.1", "::1"}, MaxItems: 128},
	}
}

// NetworkConfig validates native values without reading the environment. The
// launch layer resolves explicit environment references before creating clients.
func NetworkConfig(raw any) (harness.NetworkConfig, error) {
	var result harness.NetworkConfig
	result.Proxy.NoProxy = []string{"localhost", "127.0.0.1", "::1"}
	if raw == nil {
		return result, nil
	}
	rawNetwork, ok := raw.(map[string]any)
	if !ok {
		return result, fmt.Errorf("network: must be an object")
	}
	if rawNetwork["proxy"] == nil {
		return result, nil
	}
	proxy, ok := rawNetwork["proxy"].(map[string]any)
	if !ok {
		return result, fmt.Errorf("network.proxy: must be an object")
	}
	for _, field := range networkFields() {
		key := strings.TrimPrefix(field.Path, "/network/proxy/")
		if key == "clear_url" {
			continue
		}
		if value, exists := proxy[key]; exists {
			if err := validate(field, value); err != nil {
				return result, err
			}
		}
	}
	result.Proxy.Enabled, _ = proxy["enabled"].(bool)
	result.Proxy.URL, _ = proxy["url"].(string)
	result.Proxy.URL = strings.TrimSpace(result.Proxy.URL)
	if value, exists := proxy["no_proxy"]; exists {
		result.Proxy.NoProxy = []string{}
		switch list := value.(type) {
		case []string:
			result.Proxy.NoProxy = append(result.Proxy.NoProxy, list...)
		case []any:
			for _, item := range list {
				result.Proxy.NoProxy = append(result.Proxy.NoProxy, item.(string))
			}
		}
	}
	address := result.Proxy.URL
	if result.Proxy.Enabled && address == "" {
		return result, fmt.Errorf("network.proxy.url: an enabled proxy requires an address")
	}
	if strings.HasPrefix(address, "$") {
		if !networkEnvironmentReference.MatchString(address) {
			return result, fmt.Errorf("network.proxy.url: invalid environment reference")
		}
	}
	check := result
	if strings.HasPrefix(address, "$") {
		check.Proxy.URL = "http://environment-reference.invalid"
	}
	if err := network.Validate(check); err != nil {
		return result, fmt.Errorf("network.proxy: %w", err)
	}
	return result, nil
}

var networkEnvironmentReference = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*(:-[^\r\n\x00}]*)?\})$`)

func validateNetworkBypass(values []string) error {
	return network.Validate(harness.NetworkConfig{Proxy: harness.ProxyConfig{NoProxy: values}})
}
