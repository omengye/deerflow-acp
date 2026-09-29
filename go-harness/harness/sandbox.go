package harness

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSandboxDisabled    = errors.New("command execution is disabled")
	ErrSandboxUnavailable = errors.New("command provider is unavailable")
	ErrCommandUncertain   = errors.New("command termination could not be confirmed; do not automatically retry")
)

type SandboxProvider string

const (
	SandboxDisabled   SandboxProvider = "disabled"
	SandboxLocal      SandboxProvider = "local"
	SandboxPowerShell SandboxProvider = "powershell"
	SandboxWSL2       SandboxProvider = "wsl2"
	SandboxDocker     SandboxProvider = "docker"
)

// SandboxConfig is operator configuration, never model-supplied tool input.
// Local/PowerShell/WSL2 share host capabilities and are not security sandboxes.
// Every Start still requires the owning tool's explicit permission policy.
type SandboxConfig struct {
	Enabled  bool
	Provider SandboxProvider
	// Absolute program paths allowed for argv requests (host paths for local,
	// guest paths for Docker/WSL2). Allowlisting an interpreter grants its code.
	AllowedExecutables []string
	// AllowShell explicitly grants arbitrary scripts to the selected shell.
	// Executable allowlists do not restrict programs launched inside a script.
	AllowShell bool
	Shell      string
	// Only these explicit environment values are added; host credentials are
	// not inherited. Callers must not put values in public events or logs.
	Environment map[string]string
	Limits      CommandLimits
	Docker      DockerSandboxConfig
	WSL         WSLSandboxConfig
}

type CommandLimits struct {
	Timeout       time.Duration // default 2 minutes; request may only lower it
	OutputBytes   int           // retained bytes per stdout/stderr; default 256 KiB
	MaxConcurrent int           // default 4
	MaxRetained   int           // running + completed task records; default 64
	// Job Object limits on Windows local execution; rejected when a provider
	// cannot enforce them. Docker uses the separate container limits below.
	MemoryBytes  int64
	MaxProcesses int
	CPUPercent   uint32 // hard cap, 1..100, of host CPU capacity
}

type DockerSandboxConfig struct {
	Executable        string
	Image             string
	AllowedImages     []string
	Network           string  // none (default) or explicitly bridge; host is forbidden
	CPUs              float64 // default 1
	MemoryBytes       int64   // default 512 MiB
	PIDs              int     // default 64
	User              string  // default host UID:GID on Unix, 1000:1000 on Windows
	WorkspaceReadOnly bool
}

type WSLSandboxConfig struct {
	Executable   string
	Distribution string // required, never silently choose another distro
	User         string
}

type CommandRequest struct {
	Executable string        `json:"executable,omitempty"`
	Args       []string      `json:"args,omitempty"`
	Script     string        `json:"script,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
}

type CommandState string

const (
	CommandStarting  CommandState = "starting"
	CommandRunning   CommandState = "running"
	CommandCompleted CommandState = "completed"
	CommandFailed    CommandState = "failed"
	CommandCancelled CommandState = "cancelled"
	CommandTimedOut  CommandState = "timed_out"
	CommandUncertain CommandState = "uncertain"
)

type CommandOutput struct {
	Text       string `json:"text"`
	TotalBytes int64  `json:"totalBytes"`
	Truncated  bool   `json:"truncated"`
}

type CommandSnapshot struct {
	ID                   string          `json:"id"`
	Provider             SandboxProvider `json:"provider"`
	State                CommandState    `json:"state"`
	PID                  int             `json:"pid,omitempty"`        // host transport PID, never a remote kill ID
	ResourceID           string          `json:"resourceId,omitempty"` // e.g. owned Docker container name, for reconciliation
	Stdout               CommandOutput   `json:"stdout"`
	Stderr               CommandOutput   `json:"stderr"`
	ExitCode             *int            `json:"exitCode,omitempty"`
	TerminationConfirmed bool            `json:"terminationConfirmed"`
	StartedAt            time.Time       `json:"startedAt"`
	FinishedAt           time.Time       `json:"finishedAt,omitempty"`
	Error                string          `json:"error,omitempty"`
	// All implementations in V1 are process-local. A durable record is
	// evidence, not a restartable process handle or an instruction to retry.
	ProcessLocal bool `json:"processLocal"`
}

// CommandBackend is scoped to one fixed workspace and its owning session.
// Start's context owns task lifetime. Wait's context only limits waiting.
// Poll returns bounded snapshots, so repeated polling does not grow storage.
type CommandBackend interface {
	Start(context.Context, CommandRequest) (CommandSnapshot, error)
	Poll(context.Context, string) (CommandSnapshot, error)
	Wait(context.Context, string) (CommandSnapshot, error)
	Cancel(context.Context, string) (CommandSnapshot, error)
	Release(context.Context, string) error // completed, confirmed tasks only
	Close() error                          // cancel and join every owned process and output reader
}
