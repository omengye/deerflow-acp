package harness

import "time"

// ACPAgentConfig is an explicit host allowlist entry for one external stdio
// agent. The command must be an absolute executable path. Agents run with a
// separate working directory and a trimmed environment; this is not an OS
// sandbox and the executable itself must be trusted by the host.
type ACPAgentConfig struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Timeout     time.Duration     `json:"-"`
	// TimeoutSeconds is used by JSON configuration files. Zero defaults to 600.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}
