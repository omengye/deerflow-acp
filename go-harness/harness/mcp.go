package harness

import "time"

// MCPServer is session-scoped connection intent. Env and Headers are credentials
// and must not be persisted in ordinary history, sent to models, or logged.
type MCPServer struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
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
