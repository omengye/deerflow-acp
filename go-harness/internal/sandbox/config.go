// Package sandbox owns optional command processes. Local and WSL providers are
// not OS isolation boundaries; tool approval belongs to the harness policy.
package sandbox

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func normalizeConfig(cfg harness.SandboxConfig) (harness.SandboxConfig, error) {
	if !cfg.Enabled || cfg.Provider == "" || cfg.Provider == harness.SandboxDisabled {
		return cfg, harness.ErrSandboxDisabled
	}
	l := &cfg.Limits
	if l.Timeout < 0 || l.OutputBytes < 0 || l.MaxConcurrent < 0 || l.MaxRetained < 0 || l.MemoryBytes < 0 || l.MaxProcesses < 0 || l.CPUPercent > 100 {
		return cfg, fmt.Errorf("invalid command limits")
	}
	if l.Timeout == 0 {
		l.Timeout = 2 * time.Minute
	}
	if l.OutputBytes == 0 {
		l.OutputBytes = 256 << 10
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = 4
	}
	if l.MaxRetained == 0 {
		l.MaxRetained = 64
	}
	if l.OutputBytes > 16<<20 || l.MaxConcurrent > 64 || l.MaxRetained > 1024 || l.MaxRetained < l.MaxConcurrent {
		return cfg, fmt.Errorf("command limits exceed supported bounds")
	}
	cfg.AllowedExecutables = append([]string(nil), cfg.AllowedExecutables...)
	env := make(map[string]string, len(cfg.Environment))
	for k, v := range cfg.Environment {
		if !validEnvName(k) || strings.ContainsRune(v, 0) {
			return cfg, fmt.Errorf("invalid command environment name or value")
		}
		env[k] = v
	}
	cfg.Environment = env
	switch cfg.Provider {
	case harness.SandboxLocal:
		if len(cfg.AllowedExecutables) == 0 {
			return cfg, fmt.Errorf("local provider requires an executable allowlist")
		}
		for i, path := range cfg.AllowedExecutables {
			resolved, err := absoluteExecutable(path)
			if err != nil {
				return cfg, err
			}
			cfg.AllowedExecutables[i] = resolved
		}
	case harness.SandboxPowerShell:
		if !cfg.AllowShell {
			return cfg, fmt.Errorf("PowerShell requires explicit AllowShell")
		}
		if cfg.Shell == "" {
			path, err := exec.LookPath("pwsh")
			if err != nil && runtime.GOOS == "windows" {
				path = filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
				err = nil
			}
			if err != nil {
				return cfg, fmt.Errorf("%w: PowerShell executable not found", harness.ErrSandboxUnavailable)
			}
			cfg.Shell = path
		}
		path, err := absoluteExecutable(cfg.Shell)
		if err != nil {
			return cfg, err
		}
		cfg.Shell = path
	case harness.SandboxDocker:
		d := &cfg.Docker
		if d.Image == "" || strings.HasPrefix(d.Image, "-") || !contains(d.AllowedImages, d.Image) {
			return cfg, fmt.Errorf("Docker image must match an explicit image allowlist")
		}
		d.AllowedImages = append([]string(nil), d.AllowedImages...)
		if d.Network == "" {
			d.Network = "none"
		}
		if d.Network != "none" && d.Network != "bridge" {
			return cfg, fmt.Errorf("Docker network must be none or explicitly bridge")
		}
		if math.IsNaN(d.CPUs) || math.IsInf(d.CPUs, 0) || d.CPUs < 0 || d.MemoryBytes < 0 || d.PIDs < 0 {
			return cfg, fmt.Errorf("invalid Docker resource limits")
		}
		if d.CPUs == 0 {
			d.CPUs = 1
		}
		if d.MemoryBytes == 0 {
			d.MemoryBytes = 512 << 20
		}
		if d.PIDs == 0 {
			d.PIDs = 64
		}
		if d.User == "" {
			d.User = defaultContainerUser()
		}
		path, err := findExecutable(d.Executable, "docker")
		if err != nil {
			return cfg, err
		}
		d.Executable = path
		if cfg.Shell == "" {
			cfg.Shell = "/bin/sh"
		}
	case harness.SandboxWSL2:
		if runtime.GOOS != "windows" {
			return cfg, fmt.Errorf("%w: WSL2 requires Windows", harness.ErrSandboxUnavailable)
		}
		if cfg.WSL.Distribution == "" {
			return cfg, fmt.Errorf("WSL2 distribution must be explicit")
		}
		path, err := findExecutable(cfg.WSL.Executable, "wsl.exe")
		if err != nil {
			return cfg, err
		}
		cfg.WSL.Executable = path
		if cfg.Shell == "" {
			cfg.Shell = "/bin/sh"
		}
	default:
		return cfg, fmt.Errorf("unknown command provider %q", cfg.Provider)
	}
	if cfg.Provider == harness.SandboxDocker || cfg.Provider == harness.SandboxWSL2 {
		if l.MemoryBytes != 0 || l.MaxProcesses != 0 || l.CPUPercent != 0 {
			return cfg, fmt.Errorf("host Job limits do not apply to remote providers; configure Docker container limits")
		}
		for _, executable := range append(append([]string(nil), cfg.AllowedExecutables...), cfg.Shell) {
			if !strings.HasPrefix(executable, "/") || strings.ContainsAny(executable, "\x00\r\n") {
				return cfg, fmt.Errorf("remote executable paths must be absolute Unix paths")
			}
		}
	}
	if runtime.GOOS != "windows" && (l.MemoryBytes != 0 || l.MaxProcesses != 0 || l.CPUPercent != 0) {
		return cfg, fmt.Errorf("local resource limits require Windows Job Objects; use Docker for Unix memory/CPU/PID limits")
	}
	return cfg, nil
}

func findExecutable(path, name string) (string, error) {
	if path == "" {
		var err error
		path, err = exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("%w: %s executable not found", harness.ErrSandboxUnavailable, name)
		}
	}
	return absoluteExecutable(path)
}
func absoluteExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("executable must be an absolute path")
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable not found", harness.ErrSandboxUnavailable)
	}
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: executable is not a regular file", harness.ErrSandboxUnavailable)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("%w: executable lacks execute permission", harness.ErrSandboxUnavailable)
	}
	return p, nil
}
func contains(items []string, value string) bool {
	for _, s := range items {
		if s == value {
			return true
		}
	}
	return false
}
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
func environment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "LANG", "LC_ALL"} {
		if v := os.Getenv(key); v != "" {
			values[key] = v
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func New(ctx context.Context, cfg harness.SandboxConfig, workspace string) (*Backend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	cwd, err := session.NormalizeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	workspaceHandle, err := os.Open(cwd)
	if err != nil {
		return nil, err
	}
	info, err := workspaceHandle.Stat()
	if err != nil {
		_ = workspaceHandle.Close()
		return nil, err
	}
	b := &Backend{cfg: cfg, cwd: cwd, identity: info, workspace: workspaceHandle, tasks: map[string]*task{}, closeDone: make(chan struct{})}
	if err = b.probe(ctx); err != nil {
		_ = workspaceHandle.Close()
		return nil, err
	}
	return b, nil
}
