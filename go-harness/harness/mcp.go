package harness

import "time"

// MCPServer is session-scoped connection intent. Env and Headers are credentials
// and must not be persisted in ordinary history, sent to models, or logged.
type MCPServer struct {
	Name      string            `json:"name" yaml:"name"`
	Transport string            `json:"transport" yaml:"transport"`
	Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// HostMCPRecipe is explicit host authority for one exact workspace. The host
// owns its transports until SDK Close. It never expands client MCP policy.
// Secrets belong only in this host configuration, never execution snapshots.
type HostMCPRecipe struct {
	ID               string      `json:"id" yaml:"id"`
	Workspace        string      `json:"workspace" yaml:"workspace"`
	Servers          []MCPServer `json:"servers" yaml:"servers"`
	RestoreOnRestart bool        `json:"restore_on_restart" yaml:"restore_on_restart"`
}

// MCPPolicy authorizes process startup and optional outbound network transports.
// An empty AllowedCommands list denies all stdio process startup. Each entry
// must be an absolute path to a trusted executable; this is not an OS sandbox.
type MCPPolicy struct {
	AllowedCommands []string
	AllowHTTP       bool
	AllowSSE        bool
	ConnectTimeout  time.Duration
	CallTimeout     time.Duration
	CloseTimeout    time.Duration
	MaxServers      int
	MaxTools        int
	MaxToolPages    int
	MaxResultChars  int
}
