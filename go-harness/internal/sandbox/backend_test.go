package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCommandHelper(t *testing.T) {
	if os.Getenv("DEERFLOW_SANDBOX_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	switch args[0] {
	case "echo":
		cwd, _ := os.Getwd()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"cwd": cwd, "args": args[1:], "secret": os.Getenv("DEERFLOW_SANDBOX_HOST_SECRET"), "explicit": os.Getenv("EXPLICIT_VALUE")})
		os.Exit(7)
	case "output":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), 1<<20))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), 1<<20))
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "tree", "orphan":
		child := exec.Command(os.Args[0], "-test.run=^TestCommandHelper$", "--", "child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		hideTestProcess(child)
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = os.WriteFile("child.pid", []byte(strconv.Itoa(child.Process.Pid)), 0600)
		if args[0] == "orphan" {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "child":
		for {
			_ = os.WriteFile("heartbeat", []byte(time.Now().String()), 0600)
			time.Sleep(25 * time.Millisecond)
		}
	}
	os.Exit(2)
}

func testConfig(t *testing.T) harness.SandboxConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SANDBOX_HELPER": "1", "EXPLICIT_VALUE": "visible"}, Limits: harness.CommandLimits{Timeout: 10 * time.Second, OutputBytes: 4096}}
}
func newTestBackend(t *testing.T, cfg harness.SandboxConfig, cwd string) *Backend {
	t.Helper()
	b, err := New(context.Background(), cfg, cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("backend close: %v", err)
		}
	})
	return b
}
func helperRequest(t *testing.T, mode string) harness.CommandRequest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return harness.CommandRequest{Executable: exe, Args: []string{"-test.run=^TestCommandHelper$", "--", mode}}
}
func waitTask(t *testing.T, b *Backend, id string) harness.CommandSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := b.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func launchTask(t *testing.T, b *Backend, request harness.CommandRequest) harness.CommandSnapshot {
	t.Helper()
	s, err := b.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDefaultDisabledAndRequestPolicies(t *testing.T) {
	if _, err := New(context.Background(), harness.SandboxConfig{}, t.TempDir()); !errors.Is(err, harness.ErrSandboxDisabled) {
		t.Fatalf("default enabled: %v", err)
	}
	cfg := testConfig(t)
	b := newTestBackend(t, cfg, t.TempDir())
	for _, request := range []harness.CommandRequest{{Executable: "relative.exe"}, {Script: "echo unapproved"}, {Executable: cfg.AllowedExecutables[0], Timeout: time.Hour}, {Executable: cfg.AllowedExecutables[0], Args: []string{"nul\x00argument"}}} {
		if _, err := b.Start(context.Background(), request); err == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
	cfg.Provider = harness.SandboxPowerShell
	cfg.AllowShell = false
	if _, err := New(context.Background(), cfg, t.TempDir()); err == nil {
		t.Fatal("shell enabled without explicit grant")
	}
}

func TestLocalCWDExitArgumentsAndCredentialEnvironment(t *testing.T) {
	t.Setenv("DEERFLOW_SANDBOX_HOST_SECRET", "must-not-inherit")
	cwd := t.TempDir()
	b := newTestBackend(t, testConfig(t), cwd)
	r := helperRequest(t, "echo")
	r.Args = append(r.Args, `space value`, `quotes" and $(literal); &`, "中文")
	s := waitTask(t, b, launchTask(t, b, r).ID)
	if s.State != harness.CommandFailed || s.ExitCode == nil || *s.ExitCode != 7 || !s.TerminationConfirmed {
		t.Fatalf("outcome: %+v", s)
	}
	var echoed struct {
		CWD, Secret, Explicit string
		Args                  []string
	}
	if err := json.Unmarshal([]byte(s.Stdout.Text), &echoed); err != nil {
		t.Fatalf("%s: %v", s.Stdout.Text, err)
	}
	if !strings.EqualFold(filepath.Clean(echoed.CWD), filepath.Clean(cwd)) || echoed.Secret != "" || echoed.Explicit != "visible" {
		t.Fatalf("ambient capability or cwd mismatch: %+v", echoed)
	}
	if len(echoed.Args) != 3 || echoed.Args[1] != r.Args[len(r.Args)-2] || echoed.Args[2] != "中文" {
		t.Fatalf("argv was reinterpreted: %#v", echoed.Args)
	}
}

func TestBoundedOutputAndTaskRetention(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrent = 1
	cfg.Limits.MaxRetained = 1
	b := newTestBackend(t, cfg, t.TempDir())
	s := waitTask(t, b, launchTask(t, b, helperRequest(t, "output")).ID)
	if s.State != harness.CommandCompleted || !s.TerminationConfirmed {
		t.Fatalf("outcome: %+v", s)
	}
	for _, out := range []harness.CommandOutput{s.Stdout, s.Stderr} {
		if len(out.Text) != 4096 || out.TotalBytes != 1<<20 || !out.Truncated {
			t.Fatalf("capture not bounded: len=%d total=%d", len(out.Text), out.TotalBytes)
		}
	}
	if _, err := b.Start(context.Background(), helperRequest(t, "echo")); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("retention capacity: %v", err)
	}
	if err := b.Release(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Poll(context.Background(), s.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("released task visible: %v", err)
	}
	waitTask(t, b, launchTask(t, b, helperRequest(t, "echo")).ID)
}

func TestWaitCancellationDoesNotCancelTask(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxConcurrent = 1
	b := newTestBackend(t, cfg, t.TempDir())
	s := launchTask(t, b, helperRequest(t, "sleep"))
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := b.Wait(waitCtx, s.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	if next, err := b.Poll(context.Background(), s.ID); err != nil || next.FinishedAt.IsZero() == false {
		t.Fatalf("Wait cancelled actual task: %+v %v", next, err)
	}
	if _, err := b.Start(context.Background(), helperRequest(t, "echo")); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("active capacity not enforced: %v", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	finished, err := b.Cancel(ctx, s.ID)
	if err != nil || finished.State != harness.CommandCancelled || !finished.TerminationConfirmed {
		t.Fatalf("cancel: %+v %v", finished, err)
	}
}

func TestTimeoutAndLifetimeCancellation(t *testing.T) {
	b := newTestBackend(t, testConfig(t), t.TempDir())
	r := helperRequest(t, "sleep")
	r.Timeout = 100 * time.Millisecond
	s := waitTask(t, b, launchTask(t, b, r).ID)
	if s.State != harness.CommandTimedOut || !s.TerminationConfirmed {
		t.Fatalf("timeout: %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := b.Start(ctx, helperRequest(t, "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	s = waitTask(t, b, s.ID)
	if s.State != harness.CommandCancelled || !s.TerminationConfirmed {
		t.Fatalf("lifetime cancellation: %+v", s)
	}
}

func TestProcessTreeCancellationAndCloseJoin(t *testing.T) {
	for _, mode := range []string{"tree", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			cwd := t.TempDir()
			b := newTestBackend(t, testConfig(t), cwd)
			s := launchTask(t, b, helperRequest(t, mode))
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
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatalf("child did not start: %+v", waitTask(t, b, s.ID))
			}
			if mode == "tree" {
				if err := b.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s = waitTask(t, b, s.ID)
			if !s.TerminationConfirmed {
				t.Fatalf("termination not confirmed: %+v", s)
			}
			if processAlive(pid) {
				t.Fatalf("owned descendant %d survived cleanup", pid)
			}
		})
	}
}

func TestWorkspaceReplacementRejectedAtStart(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	b := newTestBackend(t, testConfig(t), cwd)
	if err := os.Rename(cwd, filepath.Join(base, "old")); err != nil {
		if runtime.GOOS == "windows" {
			t.Log("pinned directory handle prevents workspace replacement on Windows")
			return
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(context.Background(), helperRequest(t, "echo")); err == nil {
		t.Fatal("replaced workspace was accepted")
	}
}

func TestPowerShellRealExecution(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil && os.Getenv("SystemRoot") == "" {
		t.Skip("PowerShell is not installed")
	}
	b := newTestBackend(t, harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}, t.TempDir())
	s := waitTask(t, b, launchTask(t, b, harness.CommandRequest{Script: `[Console]::Write('中文 output'); [Console]::Error.Write('stderr'); exit 7`}).ID)
	if s.Stdout.Text != "中文 output" || s.Stderr.Text != "stderr" || s.ExitCode == nil || *s.ExitCode != 7 || !s.TerminationConfirmed {
		t.Fatalf("PowerShell outcome: %+v", s)
	}
}
