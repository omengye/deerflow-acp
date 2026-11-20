package launch

import (
	"fmt"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
)

// ResolveNetworkConfig resolves host-owned proxy references without publishing
// their values through settings snapshots, errors or execution metadata.
func ResolveNetworkConfig(raw any, values ConfigValues) (harness.NetworkConfig, error) {
	config, err := configschema.NetworkConfig(raw)
	if err != nil {
		return config, err
	}
	if strings.HasPrefix(config.Proxy.URL, "$") {
		config.Proxy.URL, err = values.Resolve(config.Proxy.URL)
		if err != nil && config.Proxy.Enabled {
			return config, fmt.Errorf("network.proxy.url: proxy reference could not be resolved: %w", err)
		}
	}
	if err := network.Validate(config); err != nil {
		return config, fmt.Errorf("network.proxy: invalid resolved proxy configuration")
	}
	return config, nil
}
