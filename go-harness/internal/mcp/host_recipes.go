package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type hostRecipe struct {
	config harness.HostMCPRecipe
	digest string
}

type hostRegistry struct {
	mu         sync.Mutex
	manager    *Manager
	recipes    map[string]hostRecipe
	workspaces map[string]string
	sessions   map[string]string
}

const hostRecipeOwner = "deerflow-host-mcp-recipes-v1"

// ConfigureHostRecipes must run before the manager is exposed. Only this
// trusted host API can create reconstructable resources. Client Bind never
// learns the host manager's executable or network authority.
func (m *Manager) ConfigureHostRecipes(configs []harness.HostMCPRecipe) error {
	if len(configs) == 0 {
		return nil
	}
	if len(configs) > 32 || m.host != nil {
		return invalid("host recipe registry is invalid")
	}
	policy := m.policy
	policy.AllowHTTP, policy.AllowSSE = false, false
	for _, c := range configs {
		for _, s := range c.Servers {
			switch s.Transport {
			case "", "stdio":
				policy.AllowedCommands = append(policy.AllowedCommands, s.Command)
			case "http":
				policy.AllowHTTP = true
			case "sse":
				policy.AllowSSE = true
			}
		}
	}
	host, err := New(policy)
	if err != nil {
		return invalid("host recipe executable policy is invalid")
	}
	r := &hostRegistry{manager: host, recipes: map[string]hostRecipe{}, workspaces: map[string]string{}, sessions: map[string]string{}}
	for _, c := range configs {
		if c.ID == "" || len(c.ID) > 128 || strings.TrimSpace(c.ID) != c.ID || strings.IndexFunc(c.ID, unicode.IsControl) >= 0 || !filepath.IsAbs(c.Workspace) || len(c.Servers) == 0 {
			_ = host.Close()
			return invalid("host recipe requires a bounded ID, absolute workspace and servers")
		}
		servers, cwd, err := host.prepare(c.Workspace, c.Servers)
		if err != nil {
			_ = host.Close()
			return invalid("host recipe connection configuration is invalid")
		}
		if _, exists := r.recipes[c.ID]; exists || r.workspaces[pathKey(cwd)] != "" {
			_ = host.Close()
			return invalid("host recipe IDs and exact workspaces must be unique")
		}
		c.Workspace, c.Servers = cwd, servers
		r.recipes[c.ID] = hostRecipe{config: c, digest: hostConfigDigest(cwd, servers)}
		r.workspaces[pathKey(cwd)] = c.ID
	}
	m.host = r
	return nil
}

func hostConfigDigest(cwd string, servers []harness.MCPServer) string {
	data, _ := json.Marshal(struct {
		CWD     string
		Servers []harness.MCPServer
	}{cwd, servers})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (m *Manager) hostBinding(id string) (*Manager, string) {
	if m.host == nil {
		return nil, ""
	}
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	if recipe := m.host.sessions[id]; recipe != "" {
		return m.host.manager, recipe
	}
	return nil, ""
}

func (m *Manager) HostRecipeID(id string) string { _, recipe := m.hostBinding(id); return recipe }

func (m *Manager) bindHost(ctx context.Context, owner, id, cwd string, servers []harness.MCPServer) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if owner == "" || id == "" {
		return false, invalid("owner and session are required")
	}
	real, err := session.NormalizeWorkspace(cwd)
	if err != nil {
		return false, err
	}
	r := m.host
	r.mu.Lock()
	defer r.mu.Unlock()
	recipeID := r.workspaces[pathKey(real)]
	if recipeID == "" {
		return false, nil
	}
	if len(servers) > 0 {
		return true, invalid("client MCP servers cannot be mixed with an explicitly configured host workspace recipe")
	}
	m.mu.Lock()
	closed := m.closed || m.retired[owner]
	m.mu.Unlock()
	if closed {
		return true, ErrClosed
	}
	if previous := r.sessions[id]; previous != "" && previous != recipeID {
		return true, harness.ErrPermissionDenied
	}
	c := r.recipes[recipeID].config
	if err = r.manager.Bind(ctx, hostRecipeOwner, recipeID, c.Workspace, c.Servers); err != nil {
		return true, err
	}
	r.sessions[id] = recipeID
	return true, nil
}

// RestoreHost only reconnects a recipe whose current host configuration still
// opts in and whose entire connection fingerprint matches saved intent. Actual
// schema is checked by Acquire. It does not change durable grants or run tasks.
func (m *Manager) RestoreHost(ctx context.Context, ref harness.MCPResourceRef, cwd string) (string, error) {
	if ref.HostRecipeID == "" || m.host == nil {
		return "", harness.ErrResourcesWaiting
	}
	r := m.host
	r.mu.Lock()
	c, exists := r.recipes[ref.HostRecipeID]
	r.mu.Unlock()
	if !exists {
		return "", harness.ErrResourcesWaiting
	}
	if !c.config.RestoreOnRestart {
		return "", harness.ErrResourcesWaiting
	}
	real, err := session.NormalizeWorkspace(cwd)
	if err != nil || pathKey(real) != pathKey(c.config.Workspace) || c.digest != ref.Descriptor.ConfigDigest {
		return "", harness.ErrResourcesIncompatible
	}
	if _, err = m.bindHost(ctx, hostRecipeOwner, ref.SessionID, real, nil); err != nil {
		return "", err
	}
	if m.HostRecipeID(ref.SessionID) != ref.HostRecipeID {
		return "", harness.ErrResourcesIncompatible
	}
	generation, err := m.Generation(ctx, ref.SessionID)
	if err != nil {
		return "", err
	}
	lease, err := m.Acquire(ctx, ref.SessionID, ref.Descriptor, generation)
	if err != nil {
		return "", errors.Join(harness.ErrResourcesWaiting, err)
	}
	_ = lease.Close()
	return generation, nil
}

// HostRecipeStatus exposes only classification, never a host recipe or secret.
func (m *Manager) HostRecipeStatus(ref harness.MCPResourceRef) string {
	if ref.HostRecipeID == "" {
		return ""
	}
	if m.host == nil {
		return "missing"
	}
	m.host.mu.Lock()
	defer m.host.mu.Unlock()
	c, ok := m.host.recipes[ref.HostRecipeID]
	if !ok {
		return "missing"
	}
	if c.digest != ref.Descriptor.ConfigDigest {
		return "incompatible"
	}
	if !c.config.RestoreOnRestart {
		return "manual"
	}
	return "restorable"
}
