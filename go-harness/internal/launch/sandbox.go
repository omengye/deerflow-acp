package launch

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Only selecting a provider enables execution. Setting a limit, an allowlist,
// or AllowShell alone never adds the execute tool to a session.
func sandboxFlags(flags *flag.FlagSet, cfg *harness.SandboxConfig) {
	if cfg.Provider == "" {
		cfg.Provider = harness.SandboxDisabled
		cfg.Enabled = false
	}
	flags.Func("sandbox-provider", "command provider: disabled (default), local, powershell, wsl2, docker; selecting a provider explicitly enables execution", func(value string) error {
		provider := harness.SandboxProvider(value)
		switch provider {
		case harness.SandboxDisabled, harness.SandboxLocal, harness.SandboxPowerShell, harness.SandboxWSL2, harness.SandboxDocker:
			cfg.Provider, cfg.Enabled = provider, provider != harness.SandboxDisabled
			return nil
		default:
			return fmt.Errorf("unknown sandbox provider")
		}
	})
	addListFlag(flags, "sandbox-allow-command", "exact executable allowed for argv requests (repeatable; absolute host path for local, absolute guest path for wsl2/docker)", &cfg.AllowedExecutables)
	flags.BoolVar(&cfg.AllowShell, "sandbox-allow-shell", cfg.AllowShell, "explicitly allow arbitrary scripts for powershell/wsl2/docker; argv allowlists do not restrict scripts")
	flags.StringVar(&cfg.Shell, "sandbox-shell", cfg.Shell, "shell executable: absolute host path for powershell, absolute guest path for wsl2/docker")
	flags.Func("sandbox-env", "explicit command environment NAME=VALUE (repeatable); ambient credentials are not inherited", func(value string) error {
		key, content, found := strings.Cut(value, "=")
		if !found || !environmentName(key) || strings.ContainsRune(content, 0) {
			return fmt.Errorf("sandbox environment must use a valid NAME=VALUE")
		}
		if cfg.Environment == nil {
			cfg.Environment = make(map[string]string)
		}
		cfg.Environment[key] = content
		return nil
	})
	flags.DurationVar(&cfg.Limits.Timeout, "sandbox-timeout", cfg.Limits.Timeout, "maximum command duration; 0 uses 2m")
	flags.IntVar(&cfg.Limits.OutputBytes, "sandbox-output-bytes", cfg.Limits.OutputBytes, "retained bytes per command stdout/stderr; 0 uses 262144")
	flags.IntVar(&cfg.Limits.MaxConcurrent, "sandbox-max-concurrent", cfg.Limits.MaxConcurrent, "concurrent command limit per run backend; 0 uses 4")
	flags.IntVar(&cfg.Limits.MaxRetained, "sandbox-max-retained", cfg.Limits.MaxRetained, "retained running/completed records per run backend; 0 uses 64")
	flags.Int64Var(&cfg.Limits.MemoryBytes, "sandbox-memory-bytes", cfg.Limits.MemoryBytes, "Windows local/PowerShell Job memory limit; 0 disables")
	flags.IntVar(&cfg.Limits.MaxProcesses, "sandbox-max-processes", cfg.Limits.MaxProcesses, "Windows local/PowerShell Job process limit; 0 disables")
	flags.Func("sandbox-cpu-percent", "Windows local/PowerShell Job CPU cap, 1..100; 0 disables", func(value string) error {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n > 100 {
			return fmt.Errorf("sandbox CPU percent must be 0..100")
		}
		cfg.Limits.CPUPercent = uint32(n)
		return nil
	})
	flags.StringVar(&cfg.WSL.Executable, "sandbox-wsl-executable", cfg.WSL.Executable, "absolute host wsl.exe path; empty discovers wsl.exe")
	flags.StringVar(&cfg.WSL.Distribution, "sandbox-wsl-distribution", cfg.WSL.Distribution, "required explicit WSL2 distribution")
	flags.StringVar(&cfg.WSL.User, "sandbox-wsl-user", cfg.WSL.User, "WSL2 user; empty uses the selected distribution's default")
	flags.StringVar(&cfg.Docker.Executable, "sandbox-docker-executable", cfg.Docker.Executable, "absolute host Docker CLI path; empty discovers docker")
	flags.StringVar(&cfg.Docker.Image, "sandbox-docker-image", cfg.Docker.Image, "Docker image; must also appear in sandbox-docker-allow-image")
	addListFlag(flags, "sandbox-docker-allow-image", "exact Docker image allowed by operator policy (repeatable)", &cfg.Docker.AllowedImages)
	flags.StringVar(&cfg.Docker.Network, "sandbox-docker-network", cfg.Docker.Network, "Docker network: none (default) or explicitly bridge")
	flags.Float64Var(&cfg.Docker.CPUs, "sandbox-docker-cpus", cfg.Docker.CPUs, "Docker CPU limit; 0 uses 1")
	flags.Int64Var(&cfg.Docker.MemoryBytes, "sandbox-docker-memory-bytes", cfg.Docker.MemoryBytes, "Docker memory limit; 0 uses 536870912")
	flags.IntVar(&cfg.Docker.PIDs, "sandbox-docker-pids", cfg.Docker.PIDs, "Docker process limit; 0 uses 64")
	flags.StringVar(&cfg.Docker.User, "sandbox-docker-user", cfg.Docker.User, "Docker UID:GID; empty uses host IDs on Unix or 1000:1000 on Windows")
	flags.BoolVar(&cfg.Docker.WorkspaceReadOnly, "sandbox-docker-workspace-read-only", cfg.Docker.WorkspaceReadOnly, "mount the session workspace read-only in Docker")
}

func addListFlag(flags *flag.FlagSet, name, usage string, target *[]string) {
	flags.Func(name, usage, func(value string) error {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s must be nonempty and contain no control separators", name)
		}
		*target = append(*target, value)
		return nil
	})
}

func environmentName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
