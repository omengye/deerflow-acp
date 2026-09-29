package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDockerArgumentsAndPolicies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxDocker, AllowShell: true, Docker: harness.DockerSandboxConfig{Executable: exe, Image: "fixture:locked", AllowedImages: []string{"fixture:locked"}}}
	cfg, err = normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(t.TempDir(), "comma, and spaces")
	payload := `printf '%s' '$(do-not-run-on-host); keep quotes'`
	args := dockerCreateArgs(cfg, cwd, "deerflow-go-fixture", harness.CommandRequest{Script: payload})
	for flag, want := range map[string]string{"--name": "deerflow-go-fixture", "--network": "none", "--cpus": "1", "--memory": "536870912", "--memory-swap": "536870912", "--pids-limit": "64", "--workdir": "/workspace"} {
		at := sliceIndex(args, flag)
		if at < 0 || at+1 >= len(args) || args[at+1] != want {
			t.Fatalf("%s missing/enforcement changed: %#v", flag, args)
		}
	}
	for _, flag := range []string{"--pull=never", "--init", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only"} {
		if sliceIndex(args, flag) < 0 {
			t.Fatalf("missing security policy %s", flag)
		}
	}
	mountAt := sliceIndex(args, "--mount")
	fields, err := csv.NewReader(strings.NewReader(args[mountAt+1])).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fields, []string{"type=bind", "source=" + cwd, "target=/workspace"}) {
		t.Fatalf("mount escaping changed workspace: %#v", fields)
	}
	if !reflect.DeepEqual(args[len(args)-5:], []string{"--entrypoint", "/bin/sh", "fixture:locked", "-c", payload}) {
		t.Fatal("script was interpolated into host command")
	}
	cfg.Docker.Network = "host"
	if _, err = normalizeConfig(cfg); err == nil {
		t.Fatal("host network accepted")
	}
	cfg.Docker.Network = "none"
	cfg.Docker.Image = "untrusted:latest"
	if _, err = normalizeConfig(cfg); err == nil {
		t.Fatal("unallowlisted image accepted")
	}
}
func sliceIndex(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return -1
}

func TestWSLArgvHasExplicitDistroAndNoShellInterpolation(t *testing.T) {
	b := &Backend{cfg: harness.SandboxConfig{WSL: harness.WSLSandboxConfig{Executable: `C:\Windows\System32\wsl.exe`, Distribution: "Ubuntu-22.04", User: "test-user"}}}
	path := `C:\work\space and 'quotes'`
	got := b.wslArgs("wslpath", "-a", "-u", path)
	want := []string{`C:\Windows\System32\wsl.exe`, "--distribution", "Ubuntu-22.04", "--user", "test-user", "--exec", "wslpath", "-a", "-u", path}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WSL argv: %#v", got)
	}
	if runtime.GOOS != "windows" {
		_, err := normalizeConfig(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxWSL2, WSL: harness.WSLSandboxConfig{Distribution: "Ubuntu-22.04"}})
		if !errors.Is(err, harness.ErrSandboxUnavailable) {
			t.Fatalf("non-Windows WSL: %v", err)
		}
	}
}

func TestPowerShellEncodingPreservesScriptLiteral(t *testing.T) {
	script := `Write-Output '中文 $() "quotes"'; exit 7`
	args := powerShellArgs("powershell.exe", script)
	if len(args) != 6 || args[4] != "-EncodedCommand" {
		t.Fatalf("PowerShell args: %#v", args)
	}
	b, err := base64.StdEncoding.DecodeString(args[5])
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	if decoded := string(utf16.Decode(units)); !strings.HasSuffix(decoded, script) {
		t.Fatal("script changed during UTF-16 argument encoding")
	}
}

func TestWorkspaceCoordinateMappingRejectsTraversal(t *testing.T) {
	cwd := t.TempDir()
	b := &Backend{cwd: cwd, cfg: harness.SandboxConfig{Provider: harness.SandboxDocker}}
	got, err := b.HostPath("/workspace/sub/result.txt")
	if err != nil || got != filepath.Join(cwd, "sub", "result.txt") {
		t.Fatalf("mapping: %s %v", got, err)
	}
	for _, path := range []string{"/etc/passwd", "/workspace/../secret", "/workspace-other/file", `/workspace/a\..\secret`, `/workspace/C:secret`} {
		if _, err = b.HostPath(path); err == nil {
			t.Fatalf("unsafe mapping accepted: %s", path)
		}
	}
}

func TestProviderMissingExecutableFailsClearly(t *testing.T) {
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxDocker, Docker: harness.DockerSandboxConfig{Executable: filepath.Join(t.TempDir(), "missing-docker"), Image: "fixture", AllowedImages: []string{"fixture"}}}
	if _, err := New(context.Background(), cfg, t.TempDir()); !errors.Is(err, harness.ErrSandboxUnavailable) {
		t.Fatalf("missing executable: %v", err)
	}
}

func TestWSLTransportExitRequiresTerminationReceipt(t *testing.T) {
	zero := 0
	hostSuccess := execution{exitCode: &zero, confirmed: true, state: harness.CommandCompleted}
	for _, text := range []string{"ordinary stderr", `marker{"state":"completed","exitCode":0,"confirmed":false}`, `marker{"state":"invented","exitCode":0,"confirmed":true}`} {
		capture := newCapture(32)
		_, _ = capture.Write([]byte(text))
		result := applyWSLReceipt(hostSuccess, capture, "marker")
		if result.confirmed || result.state != harness.CommandUncertain {
			t.Fatalf("host exit falsely confirmed guest cleanup: %+v", result)
		}
	}
	output := newCapture(4)
	_, _ = output.Write([]byte("abcdefgh\nmarker" + `{"state":"completed","exitCode":0,"confirmed":true}` + "\n"))
	result := applyWSLReceipt(hostSuccess, output, "marker")
	if !result.confirmed || result.state != harness.CommandCompleted {
		t.Fatalf("receipt in bounded tail lost: %+v", result)
	}
	if visible := output.snapshot(); visible.Text != "abcd" || visible.TotalBytes != 8 || !visible.Truncated {
		t.Fatalf("protocol receipt leaked into output metadata: %+v", visible)
	}
}

func TestWSL2RealSupervisorAndCancellation(t *testing.T) {
	distro := os.Getenv("DEERFLOW_TEST_WSL_DISTRO")
	if distro == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_WSL_DISTRO to an installed WSL2 distro")
	}
	cwd := t.TempDir()
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxWSL2, AllowShell: true, WSL: harness.WSLSandboxConfig{Distribution: distro}, Limits: harness.CommandLimits{Timeout: 20 * time.Second}}
	b := newTestBackend(t, cfg, cwd)
	s := waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `printf 'wsl-ok'; printf 'stderr-ok' >&2; exit 7`}).ID)
	if s.State != harness.CommandFailed || s.ExitCode == nil || *s.ExitCode != 7 || !s.TerminationConfirmed || s.Stdout.Text != "wsl-ok" || s.Stderr.Text != "stderr-ok" {
		t.Fatalf("WSL outcome: %+v", s)
	}
	s = launchTask(t, b, harness.CommandRequest{Script: `sleep 30 & child=$!; printf '%s' "$child" > child.pid; wait`})
	deadline := time.Now().Add(8 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(cwd, "child.pid"))
		if err == nil {
			pid, _ = strconv.Atoi(string(data))
			if pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("WSL child not started: %+v", waitTask(t, b, s.ID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := b.Cancel(ctx, s.ID)
	if err != nil || s.State != harness.CommandCancelled || !s.TerminationConfirmed {
		t.Fatalf("WSL cancel: %+v %v", s, err)
	}
	out, err := b.control(context.Background(), b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "python3", "-c", "import os,sys; print(os.path.exists('/proc/'+sys.argv[1]))", strconv.Itoa(pid)))
	if err != nil || strings.TrimSpace(out) != "False" {
		t.Fatalf("WSL descendant survived: %q %v", out, err)
	}
}

func TestDockerRealContainerLifecycle(t *testing.T) {
	image := os.Getenv("DEERFLOW_TEST_DOCKER_IMAGE")
	if image == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_DOCKER_IMAGE to a pre-pulled Linux image with /bin/sh")
	}
	cwd := t.TempDir()
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxDocker, AllowShell: true, Docker: harness.DockerSandboxConfig{Image: image, AllowedImages: []string{image}}, Limits: harness.CommandLimits{Timeout: 20 * time.Second}}
	b := newTestBackend(t, cfg, cwd)
	s := waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `printf 'container-ok'; pwd`}).ID)
	if !s.TerminationConfirmed || s.ExitCode == nil || *s.ExitCode != 0 || !strings.Contains(s.Stdout.Text, "container-ok/workspace") {
		t.Fatalf("Docker outcome: %+v", s)
	}
	s = launchTask(t, b, harness.CommandRequest{Script: `sleep 30`})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := b.Cancel(ctx, s.ID)
	if err != nil || !s.TerminationConfirmed {
		t.Fatalf("Docker cancellation: %+v %v", s, err)
	}
}
