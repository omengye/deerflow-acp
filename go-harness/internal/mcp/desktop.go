package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type desktopGrant struct {
	path, target string
	args         []string
	env          map[string]string
}

func (g desktopGrant) matches(s harness.MCPServer) bool {
	return g.path == pathKey(filepath.Clean(s.Command)) && slices.Equal(g.args, s.Args) && maps.Equal(g.env, s.Env)
}

type desktopPolicy struct {
	path     string
	mu       sync.Mutex
	revision [sha256.Size]byte
	saved    map[string]desktopGrant
}

// SetDesktopProfile is called once, before exposing the manager. Only the
// launcher's selected profile supplies authority; ACP requests supply intent.
func (m *Manager) SetDesktopProfile(profile string) {
	if profile != "" {
		m.desktop = &desktopPolicy{path: filepath.Join(profile, "config", "client-mcp-servers.json")}
	}
}

// An atomic settings revision authorizes the saved launch definition. Installers
// can replace the program at the same resolved path between any two turns.
func (p *desktopPolicy) grants() (map[string]desktopGrant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	file, err := os.Open(p.path)
	if errors.Is(err, os.ErrNotExist) {
		p.saved = nil
		return nil, nil
	}
	if err != nil {
		return nil, invalid("cannot read desktop servers")
	}
	defer file.Close()
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(body) > limit {
		return nil, invalid("cannot read desktop servers within size limit")
	}
	revision := sha256.Sum256(body)
	if p.saved != nil && p.revision == revision {
		return p.saved, nil
	}
	var entries []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Env     []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&entries) != nil || entries == nil || len(entries) > 8 || dec.Decode(new(any)) != io.EOF {
		return nil, invalid("invalid desktop server definitions")
	}
	grants := make(map[string]desktopGrant, len(entries))
	for _, server := range entries {
		if _, exists := grants[server.Name]; exists || server.Name == "" || (server.Type != "" && server.Type != "stdio") {
			return nil, invalid("invalid desktop stdio server")
		}
		if !filepath.IsAbs(server.Command) {
			return nil, invalid("desktop MCP program requires an absolute path")
		}
		path := filepath.Clean(server.Command)
		target := path
		if old, ok := p.saved[server.Name]; ok && old.path == pathKey(path) {
			target = old.target
		} else if real, err := filepath.EvalSymlinks(path); err == nil {
			target = real
		}
		env := make(map[string]string, len(server.Env))
		for _, pair := range server.Env {
			if _, exists := env[pair.Name]; exists {
				return nil, invalid("duplicate desktop environment variable")
			}
			env[pair.Name] = pair.Value
		}
		grants[server.Name] = desktopGrant{path: pathKey(path), target: target, args: server.Args, env: env}
	}
	p.revision, p.saved = revision, grants
	return grants, nil
}
