package launch

import (
	"flag"
	"io"
	"reflect"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func parseRuntimeFlags(t *testing.T, args ...string) (deerflow.Config, error) {
	t.Helper()
	var cfg deerflow.Config
	flags := flag.NewFlagSet("runtime", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	RuntimeFlags(flags, &cfg)
	return cfg, flags.Parse(args)
}

func TestSandboxFlagsRequireExplicitProvider(t *testing.T) {
	for _, args := range [][]string{nil, {"--sandbox-allow-command=/usr/bin/env", "--sandbox-allow-shell", "--sandbox-timeout=3s"}, {"--sandbox-provider=local", "--sandbox-provider=disabled"}} {
		cfg, err := parseRuntimeFlags(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Sandbox.Enabled || cfg.Sandbox.Provider != harness.SandboxDisabled {
			t.Fatalf("implicitly enabled: %+v", cfg.Sandbox)
		}
	}
}

func TestSandboxProviderAndLimitFlags(t *testing.T) {
	cfg, err := parseRuntimeFlags(t, "--sandbox-provider=powershell", "--sandbox-allow-command=C:\\tools\\first.exe", "--sandbox-allow-command=C:\\tools\\second.exe", "--sandbox-allow-shell", "--sandbox-shell=C:\\tools\\pwsh.exe", "--sandbox-env=EXPLICIT=value=with=equals", "--sandbox-env=EMPTY=", "--sandbox-timeout=45s", "--sandbox-output-bytes=1000", "--sandbox-max-concurrent=2", "--sandbox-max-retained=8", "--sandbox-memory-bytes=64000", "--sandbox-max-processes=3", "--sandbox-cpu-percent=25")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Sandbox
	if !s.Enabled || s.Provider != harness.SandboxPowerShell || !s.AllowShell || s.Shell != `C:\tools\pwsh.exe` || !reflect.DeepEqual(s.AllowedExecutables, []string{`C:\tools\first.exe`, `C:\tools\second.exe`}) {
		t.Fatalf("sandbox=%+v", s)
	}
	if s.Environment["EXPLICIT"] != "value=with=equals" || s.Environment["EMPTY"] != "" || len(s.Environment) != 2 {
		t.Fatalf("environment=%v", s.Environment)
	}
	want := harness.CommandLimits{Timeout: 45 * time.Second, OutputBytes: 1000, MaxConcurrent: 2, MaxRetained: 8, MemoryBytes: 64000, MaxProcesses: 3, CPUPercent: 25}
	if s.Limits != want {
		t.Fatalf("limits=%+v", s.Limits)
	}
}

func TestRemoteSandboxFlags(t *testing.T) {
	cfg, err := parseRuntimeFlags(t, "--sandbox-provider=wsl2", "--sandbox-wsl-executable=C:\\Windows\\System32\\wsl.exe", "--sandbox-wsl-distribution=Ubuntu-22.04", "--sandbox-wsl-user=worker", "--sandbox-allow-command=/usr/bin/python3")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox.Enabled || cfg.Sandbox.Provider != harness.SandboxWSL2 || cfg.Sandbox.WSL.Distribution != "Ubuntu-22.04" || cfg.Sandbox.WSL.User != "worker" || cfg.Sandbox.WSL.Executable != `C:\Windows\System32\wsl.exe` || cfg.Sandbox.AllowedExecutables[0] != "/usr/bin/python3" {
		t.Fatalf("wsl=%+v", cfg.Sandbox)
	}
	cfg, err = parseRuntimeFlags(t, "--sandbox-provider=docker", "--sandbox-docker-executable=C:\\docker.exe", "--sandbox-docker-image=busybox:1", "--sandbox-docker-allow-image=busybox:1", "--sandbox-docker-allow-image=alpine:3", "--sandbox-docker-network=bridge", "--sandbox-docker-cpus=0.5", "--sandbox-docker-memory-bytes=67108864", "--sandbox-docker-pids=12", "--sandbox-docker-user=1001:1001", "--sandbox-docker-workspace-read-only")
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Sandbox.Docker
	if !cfg.Sandbox.Enabled || cfg.Sandbox.Provider != harness.SandboxDocker || d.Executable != `C:\docker.exe` || d.Image != "busybox:1" || !reflect.DeepEqual(d.AllowedImages, []string{"busybox:1", "alpine:3"}) || d.Network != "bridge" || d.CPUs != 0.5 || d.MemoryBytes != 67108864 || d.PIDs != 12 || d.User != "1001:1001" || !d.WorkspaceReadOnly {
		t.Fatalf("docker=%+v", d)
	}
}

func TestSandboxFlagsRejectMalformedOperatorInput(t *testing.T) {
	for _, arg := range []string{"--sandbox-provider=auto", "--sandbox-provider=", "--sandbox-allow-command=", "--sandbox-docker-allow-image= ", "--sandbox-env=NO_EQUALS", "--sandbox-env=1BAD=value", "--sandbox-env=BAD-NAME=value", "--sandbox-cpu-percent=-1", "--sandbox-cpu-percent=101", "--sandbox-cpu-percent=4294967296"} {
		if _, err := parseRuntimeFlags(t, arg); err == nil {
			t.Fatalf("accepted %q", arg)
		}
	}
}
