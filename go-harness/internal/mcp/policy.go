package mcp

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func pathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
func executable(command string) (string, os.FileInfo, error) {
	if !filepath.IsAbs(command) {
		return "", nil, fmt.Errorf("absolute command required")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(command))
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0) {
		return "", nil, fmt.Errorf("regular executable required")
	}
	return real, info, nil
}

func invalid(message string) error { return fmt.Errorf("%w: MCP %s", harness.ErrInvalidInput, message) }

func (m *Manager) prepare(cwd string, servers []harness.MCPServer) ([]harness.MCPServer, string, error) {
	if len(servers) > m.policy.MaxServers {
		return nil, "", invalid("server count exceeds configured limit")
	}
	realCWD, err := session.NormalizeWorkspace(cwd)
	if err != nil {
		return nil, "", invalid("workspace is invalid")
	}
	seen := make(map[string]bool, len(servers))
	out := make([]harness.MCPServer, 0, len(servers))
	for _, in := range servers {
		s := in
		s.Args = slices.Clone(in.Args)
		s.Env = maps.Clone(in.Env)
		s.Headers = maps.Clone(in.Headers)
		if s.Name == "" || len(s.Name) > 128 || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 {
			return nil, "", invalid("server name is invalid")
		}
		if seen[s.Name] {
			return nil, "", invalid("server names must be unique")
		}
		seen[s.Name] = true
		if s.Transport == "" {
			s.Transport = "stdio"
		}
		if len(s.Args) > 256 || len(s.Env) > 128 || len(s.Headers) > 64 {
			return nil, "", invalid("configuration exceeds configured bounds")
		}
		for _, arg := range s.Args {
			if len(arg) > 64<<10 || strings.ContainsRune(arg, 0) {
				return nil, "", invalid("command argument is invalid")
			}
		}
		for k, v := range s.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || len(k) > 256 || len(v) > 64<<10 {
				return nil, "", invalid("environment entry is invalid")
			}
		}
		headers := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			if !validHeaderName(k) || strings.ContainsAny(v, "\r\n\x00") || len(v) > 64<<10 {
				return nil, "", invalid("header entry is invalid")
			}
			name := http.CanonicalHeaderKey(k)
			if _, ok := headers[name]; ok {
				return nil, "", invalid("duplicate header name")
			}
			switch strings.ToLower(k) {
			case "host", "content-length", "connection", "transfer-encoding", "upgrade", "mcp-session-id", "mcp-protocol-version":
				return nil, "", invalid("reserved transport header")
			}
			headers[name] = v
		}
		s.Headers = headers
		switch s.Transport {
		case "stdio":
			if s.URL != "" || len(s.Headers) > 0 {
				return nil, "", invalid("stdio configuration cannot contain URL or headers")
			}
			real, info, err := executable(s.Command)
			if err != nil {
				return nil, "", fmt.Errorf("%w: MCP command must resolve to an absolute executable", harness.ErrPermissionDenied)
			}
			trusted, allowed := m.allowed[pathKey(real)]
			if !allowed || !os.SameFile(trusted, info) {
				return nil, "", fmt.Errorf("%w: MCP command is not in the trusted executable allowlist", harness.ErrPermissionDenied)
			}
			s.Command = real
		case "http", "sse":
			if s.Transport == "http" && !m.policy.AllowHTTP || s.Transport == "sse" && !m.policy.AllowSSE {
				return nil, "", fmt.Errorf("%w: MCP network transport is disabled", harness.ErrPermissionDenied)
			}
			if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 {
				return nil, "", invalid("network configuration cannot contain command or environment")
			}
			u, err := url.Parse(s.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
				return nil, "", invalid("endpoint must be an absolute HTTP(S) URL without userinfo or fragment")
			}
		default:
			return nil, "", invalid("transport must be stdio, http or sse")
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, realCWD, nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

// Deliberately do not inherit arbitrary host environment: model provider keys
// and credentials belonging to another MCP server must not reach this process.
func processEnv(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, key := range []string{"PATH", "PATHEXT", "HOME", "USERPROFILE", "SystemRoot", "WINDIR", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA"} {
		if v, ok := os.LookupEnv(key); ok {
			values[key] = v
		}
	}
	for k, v := range overrides {
		if runtime.GOOS == "windows" {
			for old := range values {
				if strings.EqualFold(old, k) {
					delete(values, old)
				}
			}
		}
		values[k] = v
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+values[k])
	}
	return result
}
