package configschema

import (
	"strings"
	"testing"
)

func TestNetworkProxyReferences(t *testing.T) {
	proxy := func(address string) any {
		return map[string]any{"proxy": map[string]any{"enabled": true, "url": address}}
	}
	for _, address := range []string{"$PROXY_URL", "${PROXY_URL}", "${PROXY_URL:-http://proxy.example:8080}", "${secret:PROXY_URL}", "http://user:pass@proxy.example:8080"} {
		config, err := NetworkConfig(proxy(address))
		if err != nil || config.Proxy.URL != address {
			t.Errorf("NetworkConfig(%q) = %q, %v", address, config.Proxy.URL, err)
		}
	}
	for address, want := range map[string]string{"${secret:lower}": "invalid secret reference", "${secret:}": "invalid secret reference", "${secret:A:-x}": "invalid secret reference", "$secret:A": "invalid environment reference", "${secret:A}x": "invalid environment reference"} {
		if _, err := NetworkConfig(proxy(address)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("NetworkConfig(%q) = %v; want %q", address, err, want)
		}
	}
}
