package desktopconfig

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
)

func networkDocument(raw any) (map[string]any, error) {
	config, err := configschema.NetworkConfig(raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{"proxy": map[string]any{
		"enabled": config.Proxy.Enabled, "url": config.Proxy.URL, "url_configured": config.Proxy.URL != "",
		"clear_url": false, "no_proxy": config.Proxy.NoProxy,
	}}, nil
}

func validatedNetwork(incoming any, previous map[string]any) (map[string]any, error) {
	// Older clients do not project this section. An unrelated save must retain it.
	if incoming == nil {
		return cloneMap(previous), nil
	}
	draft, ok := incoming.(map[string]any)
	if !ok || len(draft) != 1 {
		return nil, errors.New("network: expected a proxy configuration")
	}
	proxy, ok := draft["proxy"].(map[string]any)
	if !ok {
		return nil, errors.New("network.proxy: must be an object")
	}
	for key, value := range proxy {
		if key == "url_configured" {
			if _, ok := value.(bool); !ok {
				return nil, errors.New("network.proxy.url_configured: must be a boolean")
			}
			continue
		}
		if err := configschema.ValidateValue("/network/proxy/"+key, value); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"enabled", "url", "clear_url", "no_proxy"} {
		if _, ok := proxy[key]; !ok {
			return nil, errors.New("network.proxy: incomplete configuration")
		}
	}
	merged := cloneMap(previous)
	prior := cloneMap(readObject(previous, "proxy"))
	address := strings.TrimSpace(str(proxy, "url", ""))
	if address == redacted {
		return nil, errors.New("network.proxy.url: re-enter the proxy address")
	}
	if boolValue(proxy, "clear_url", false) {
		delete(prior, "url")
	} else if address != "" {
		prior["url"] = address
	}
	prior["enabled"], prior["no_proxy"] = proxy["enabled"], proxy["no_proxy"]
	merged["proxy"] = prior
	if _, err := configschema.NetworkConfig(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func (s service) probeNetworkConfig(request, saved map[string]any) (harness.NetworkConfig, error) {
	merged, err := validatedNetwork(request["network"], readObject(saved, "network"))
	if err != nil {
		return harness.NetworkConfig{}, err
	}
	return launch.ResolveNetworkConfig(merged, s.configValues(merged))
}

func (s service) testNetwork(request map[string]any) (map[string]any, error) {
	saved, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	config, err := s.probeNetworkConfig(request, saved)
	if err != nil {
		return nil, err
	}
	if !config.Proxy.Enabled {
		return nil, errors.New("请先启用统一代理并填写代理地址")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	err = network.Probe(ctx, config)
	result := map[string]any{"ok": err == nil, "latency_ms": time.Since(started).Milliseconds()}
	if err != nil {
		result["error_type"] = "ProxyConnectionError"
		result["note"] = "代理连接失败，请检查地址、认证和网络；测试最长 10 秒"
	} else {
		result["note"] = "已通过代理连接 https://example.com；未调用模型或保存配置"
	}
	return result, nil
}
