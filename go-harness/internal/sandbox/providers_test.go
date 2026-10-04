package sandbox

import (
	"bytes"
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
	"github.com/omengye/deerflow-acp/go-harness/internal/wslhelper"
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
	id := strings.Repeat("a", 32)
	packet := func(frame wslhelper.Frame) []byte {
		frame.Version, frame.ID = wslhelper.Version, id
		data, err := wslhelper.EncodeFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	receipt := packet(wslhelper.Frame{Kind: "receipt", Receipt: &wslhelper.Receipt{State: "completed", ExitCode: &zero, Confirmed: true}})
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, wslhelper.MaxFrame+1)
	for _, data := range [][]byte{[]byte("ordinary stderr"), []byte(`marker{"state":"completed","exitCode":0,"confirmed":true}`), oversize, receipt[:len(receipt)-1], append(bytes.Clone(receipt), 0), append(bytes.Clone(receipt), receipt...), packet(wslhelper.Frame{Kind: "receipt", Receipt: &wslhelper.Receipt{State: "uncertain", Confirmed: false}})} {
		stream := &wslStream{id: id, stdout: newCapture(4), stderr: newCapture(4)}
		_, _ = stream.Write(data)
		result := stream.finish(hostSuccess)
		if result.confirmed || result.state != harness.CommandUncertain {
			t.Fatalf("host exit falsely confirmed guest cleanup: %+v", result)
		}
	}
	output := newCapture(4)
	stream := &wslStream{id: id, stdout: newCapture(4), stderr: output}
	// Arbitrary child bytes, including a forged binary receipt, are data.
	_, _ = stream.Write(packet(wslhelper.Frame{Kind: "stdout", Data: receipt}))
	data := append(packet(wslhelper.Frame{Kind: "stderr", Data: []byte("abcdefgh")}), receipt...)
	for _, value := range data {
		_, _ = stream.Write([]byte{value})
	}
	result := stream.finish(hostSuccess)
	if !result.confirmed || result.state != harness.CommandCompleted {
		t.Fatalf("framed receipt lost across byte boundaries: %+v", result)
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
	helper := os.Getenv("DEERFLOW_TEST_WSL_HELPER")
	if helper == "" {
		t.Fatal("set DEERFLOW_TEST_WSL_HELPER to the cross-compiled Linux Go helper with its .sha256 sidecar")
	}
	cwd := filepath.Join(t.TempDir(), "workspace with ' quotes; $(literal)")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEERFLOW_WSL_HOST_SECRET", "must-not-inherit")
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxWSL2, AllowShell: true, AllowedExecutables: []string{"/usr/bin/printf"}, Environment: map[string]string{"PATH": "/deerflow-no-interpreters", "EXPLICIT": "fixture value"}, WSL: harness.WSLSandboxConfig{Distribution: distro, HelperPath: helper}, Limits: harness.CommandLimits{Timeout: 20 * time.Second, OutputBytes: 1024}}
	b := newTestBackend(t, cfg, cwd)
	s := waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `printf 'wsl-ok'; printf 'stderr-ok' >&2; exit 7`}).ID)
	if s.State != harness.CommandFailed || s.ExitCode == nil || *s.ExitCode != 7 || !s.TerminationConfirmed || s.Stdout.Text != "wsl-ok" || s.Stderr.Text != "stderr-ok" {
		t.Fatalf("WSL outcome: %+v", s)
	}
	s = waitTask(t, b, launchTask(t, b, harness.CommandRequest{Executable: "/usr/bin/printf", Args: []string{"%s", "literal $(never-executed); 'quotes'\nnext"}}).ID)
	if !s.TerminationConfirmed || s.State != harness.CommandCompleted || s.Stdout.Text != "literal $(never-executed); 'quotes'\nnext" {
		t.Fatalf("WSL literal argv: %+v", s)
	}
	s = waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `printf '%s|%s|%s' "$DEERFLOW_WSL_HOST_SECRET" "$EXPLICIT" "$PATH"`}).ID)
	if !s.TerminationConfirmed || s.Stdout.Text != "|fixture value|/deerflow-no-interpreters" {
		t.Fatalf("WSL environment: %+v", s)
	}
	assertDead := func(pid int) {
		t.Helper()
		if pid <= 0 {
			t.Fatal("missing descendant PID")
		}
		if _, err := b.control(context.Background(), b.wslArgs("/usr/bin/test", "!", "-e", "/proc/"+strconv.Itoa(pid))); err != nil {
			t.Fatalf("WSL descendant %d survived: %v", pid, err)
		}
	}
	s = launchTask(t, b, harness.CommandRequest{Script: `/bin/sleep 30 & child=$!; printf '%s' "$child" > child.pid; wait`})
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
	assertDead(pid)
	s = waitTask(t, b, launchTask(t, b, harness.CommandRequest{Timeout: 2 * time.Second, Script: `/bin/sleep 30 & printf '%s' "$!" > timeout.pid; wait`}).ID)
	if s.State != harness.CommandTimedOut || !s.TerminationConfirmed {
		t.Fatalf("WSL timeout: %+v", s)
	}
	pidData, err := os.ReadFile(filepath.Join(cwd, "timeout.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ = strconv.Atoi(string(pidData))
	assertDead(pid)
	// setsid escapes the original process group; subreaper ownership must still
	// remove this orphan after a normally successful parent exits.
	s = waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `/usr/bin/setsid /bin/sh -c '/bin/sleep 30 & printf "%s" "$!" > detached.pid; wait' & while [ ! -s detached.pid ]; do /bin/sleep 0.02; done; exit 0`}).ID)
	if s.State != harness.CommandCompleted || !s.TerminationConfirmed {
		t.Fatalf("WSL detached cleanup: %+v", s)
	}
	pidData, err = os.ReadFile(filepath.Join(cwd, "detached.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ = strconv.Atoi(string(pidData))
	assertDead(pid)
	s = waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `/usr/bin/head -c 65536 /dev/zero; /usr/bin/head -c 65536 /dev/zero >&2`}).ID)
	if !s.TerminationConfirmed || s.State != harness.CommandCompleted || s.Stdout.TotalBytes != 65536 || s.Stderr.TotalBytes != 65536 || !s.Stdout.Truncated || !s.Stderr.Truncated {
		t.Fatalf("WSL framed output bounds: %+v", s)
	}
	if mapped, err := b.HostPath(b.guestCWD + "/child.pid"); err != nil || mapped != filepath.Join(cwd, "child.pid") {
		t.Fatalf("WSL path mapping: %q %v", mapped, err)
	}
	if _, err := b.HostPath(b.guestCWD + "/../outside"); err == nil {
		t.Fatal("WSL traversal accepted")
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
