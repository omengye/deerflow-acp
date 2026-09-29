package agent

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

// The SDK's union decoder intentionally accepts fallback variants. Validate the
// discriminator and required arrays before using its typed fields. In particular,
// an unknown transport must not silently turn into stdio or HTTP.
func (a *Agent) mcpServers(raw json.RawMessage) ([]harness.MCPServer, error) {
	invalid := func() ([]harness.MCPServer, error) {
		return nil, rpcError(protocol.InvalidParams, "invalid MCP server configuration")
	}
	var request struct {
		Servers json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &request) != nil || !isArray(request.Servers) {
		return invalid()
	}
	var values []json.RawMessage
	if json.Unmarshal(request.Servers, &values) != nil {
		return invalid()
	}
	servers := make([]harness.MCPServer, 0, len(values))
	names := make(map[string]bool)
	httpEnabled, sseEnabled := a.service.MCPCapabilities()
	for _, value := range values {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			return invalid()
		}
		var kind string
		if field, ok := fields["type"]; ok {
			if !isString(field) || json.Unmarshal(field, &kind) != nil {
				return invalid()
			}
		}
		if !isString(fields["name"]) {
			return invalid()
		}
		var typed acp.McpServer
		if json.Unmarshal(value, &typed) != nil {
			return invalid()
		}
		var server harness.MCPServer
		switch kind {
		case "", "stdio":
			if typed.Stdio == nil || !isString(fields["command"]) || !stringArray(fields["args"]) || !namedStringPairs(fields["env"]) {
				return invalid()
			}
			v := typed.Stdio
			if !filepath.IsAbs(v.Command) {
				return invalid()
			}
			server = harness.MCPServer{Name: v.Name, Transport: "stdio", Command: v.Command, Args: append([]string(nil), v.Args...), Env: make(map[string]string)}
			seen := make(map[string]bool)
			for _, env := range v.Env {
				key := env.Name
				if runtime.GOOS == "windows" {
					key = strings.ToUpper(key)
				}
				if env.Name == "" || strings.ContainsAny(env.Name, "=\x00") || strings.ContainsRune(env.Value, 0) || seen[key] {
					return invalid()
				}
				seen[key] = true
				server.Env[env.Name] = env.Value
			}
		case "http", "sse":
			if !isString(fields["url"]) || !namedStringPairs(fields["headers"]) {
				return invalid()
			}
			var headers []acp.HttpHeader
			if kind == "http" {
				if !httpEnabled {
					return nil, rpcError(protocol.InvalidParams, "HTTP MCP transport is disabled")
				}
				if typed.Http == nil {
					return invalid()
				}
				v := typed.Http
				server = harness.MCPServer{Name: v.Name, Transport: kind, URL: v.Url}
				headers = v.Headers
			} else {
				if !sseEnabled {
					return nil, rpcError(protocol.InvalidParams, "SSE MCP transport is disabled")
				}
				if typed.Sse == nil {
					return invalid()
				}
				v := typed.Sse
				server = harness.MCPServer{Name: v.Name, Transport: kind, URL: v.Url}
				headers = v.Headers
			}
			server.Headers = make(map[string]string)
			parsed, parseErr := url.Parse(server.URL)
			if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return invalid()
			}
			seen := make(map[string]bool)
			for _, header := range headers {
				key := strings.ToLower(header.Name)
				if !validHeaderName(header.Name) || strings.ContainsAny(header.Value, "\r\n\x00") || seen[key] {
					return invalid()
				}
				seen[key] = true
				server.Headers[header.Name] = header.Value
			}
		default:
			return nil, rpcError(protocol.InvalidParams, "unsupported MCP transport")
		}
		if strings.TrimSpace(server.Name) == "" || strings.ContainsRune(server.Name, 0) || names[server.Name] {
			return invalid()
		}
		names[server.Name] = true
		servers = append(servers, server)
	}
	return servers, nil
}

func isArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

func isString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

func stringArray(raw json.RawMessage) bool {
	if !isArray(raw) {
		return false
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	for _, item := range items {
		if !isString(item) {
			return false
		}
	}
	return true
}

func namedStringPairs(raw json.RawMessage) bool {
	if !isArray(raw) {
		return false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	for _, item := range items {
		if !isString(item["name"]) || !isString(item["value"]) {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}
