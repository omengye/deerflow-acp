package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (b *Backend) probe(ctx context.Context) error {
	switch b.cfg.Provider {
	case harness.SandboxDocker:
		out, err := b.control(ctx, []string{b.cfg.Docker.Executable, "version", "--format", "{{.Server.Os}}"})
		if err != nil || strings.TrimSpace(out) != "linux" {
			return fmt.Errorf("%w: Docker Linux daemon is required", harness.ErrSandboxUnavailable)
		}
		_, err = b.control(ctx, []string{b.cfg.Docker.Executable, "image", "inspect", b.cfg.Docker.Image, "--format", "{{.Id}}"})
		if err != nil {
			return fmt.Errorf("%w: configured Docker image is not present; pull it explicitly", harness.ErrSandboxUnavailable)
		}
	case harness.SandboxWSL2:
		if strings.HasPrefix(b.cwd, `\\`) {
			return fmt.Errorf("WSL2 backend does not support UNC workspaces")
		}
		probe := `import json,platform,os,signal;print(json.dumps({'kernel':platform.release(),'pidfd':hasattr(os,'pidfd_open') and hasattr(signal,'pidfd_send_signal')}))`
		out, err := b.control(ctx, b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "python3", "-c", probe))
		var status struct {
			Kernel string `json:"kernel"`
			Pidfd  bool   `json:"pidfd"`
		}
		if err != nil || json.Unmarshal([]byte(out), &status) != nil || !status.Pidfd || !strings.Contains(strings.ToLower(status.Kernel), "microsoft-standard") && !strings.Contains(strings.ToLower(status.Kernel), "wsl2") {
			return fmt.Errorf("%w: explicit WSL2 distro with Python 3.9+, pidfd support and Linux /proc is required", harness.ErrSandboxUnavailable)
		}
		out, err = b.control(ctx, b.wslArgs("wslpath", "-a", "-u", b.cwd))
		if err != nil {
			return fmt.Errorf("%w: wslpath could not translate workspace", harness.ErrSandboxUnavailable)
		}
		guest := strings.TrimSpace(out)
		if !strings.HasPrefix(guest, "/") || strings.ContainsAny(guest, "\r\n\x00") {
			return fmt.Errorf("%w: wslpath returned an invalid absolute path", harness.ErrSandboxUnavailable)
		}
		out, err = b.control(ctx, b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "python3", "-c", `import os,sys; p=sys.argv[1]; assert os.path.isdir(p); print(os.path.realpath(p))`, guest))
		if err != nil || strings.TrimSpace(out) == "" {
			return fmt.Errorf("%w: translated WSL workspace is not accessible", harness.ErrSandboxUnavailable)
		}
		b.guestCWD = strings.TrimSpace(out)
	}
	return nil
}

func (b *Backend) control(parent context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	out, errOut := newCapture(64<<10), newCapture(4096)
	r := runProcess(ctx, processSpec{argv: argv, env: environment(nil), cwd: b.cwd}, out, errOut)
	if !r.confirmed || r.err != nil || r.exitCode == nil || *r.exitCode != 0 {
		return "", fmt.Errorf("provider control command failed")
	}
	if out.snapshot().Truncated {
		return "", fmt.Errorf("provider control response exceeds limit")
	}
	return out.snapshot().Text, nil
}

func (b *Backend) run(ctx context.Context, t *task, r harness.CommandRequest) execution {
	if err := b.checkWorkspace(); err != nil {
		return execution{confirmed: true, state: harness.CommandFailed, err: err}
	}
	switch b.cfg.Provider {
	case harness.SandboxDocker:
		return b.runDocker(ctx, t, r)
	case harness.SandboxWSL2:
		return b.runWSL(ctx, t, r)
	default:
		argv := append([]string{r.Executable}, r.Args...)
		if b.cfg.Provider == harness.SandboxPowerShell {
			argv = powerShellArgs(b.cfg.Shell, r.Script)
		}
		return runProcess(ctx, b.processSpec(t, argv, false), t.stdout, t.stderr)
	}
}
func (b *Backend) processSpec(t *task, argv []string, remote bool) processSpec {
	return processSpec{argv: argv, env: environment(b.cfg.Environment), cwd: b.cwd, limits: b.cfg.Limits, remoteInput: remote, started: func(pid int) {
		t.mu.Lock()
		t.snapshot.PID = pid
		t.snapshot.State = harness.CommandRunning
		t.mu.Unlock()
	}}
}

func powerShellArgs(shell, script string) []string {
	preamble := "$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false); $OutputEncoding=[Console]::OutputEncoding; "
	units := utf16.Encode([]rune(preamble + script))
	encoded := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(encoded[2*i:], u)
	}
	return []string{shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded)}
}

func remoteCommand(cfg harness.SandboxConfig, r harness.CommandRequest) []string {
	if r.Script != "" {
		return []string{cfg.Shell, "-c", r.Script}
	}
	return append([]string{r.Executable}, r.Args...)
}

func dockerCreateArgs(cfg harness.SandboxConfig, cwd, name string, r harness.CommandRequest) []string {
	var mount strings.Builder
	writer := csv.NewWriter(&mount)
	fields := []string{"type=bind", "source=" + cwd, "target=/workspace"}
	if cfg.Docker.WorkspaceReadOnly {
		fields = append(fields, "readonly")
	}
	_ = writer.Write(fields)
	writer.Flush()
	d := cfg.Docker
	args := []string{d.Executable, "create", "--name", name, "--pull=never", "--init", "--network", d.Network, "--cpus", strconv.FormatFloat(d.CPUs, 'f', -1, 64), "--memory", strconv.FormatInt(d.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(d.MemoryBytes, 10), "--pids-limit", strconv.Itoa(d.PIDs), "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=67108864", "--user", d.User, "--workdir", "/workspace", "--mount", strings.TrimSuffix(mount.String(), "\n")}
	keys := make([]string, 0, len(cfg.Environment))
	for key := range cfg.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+cfg.Environment[key])
	}
	command := remoteCommand(cfg, r)
	args = append(args, "--entrypoint", command[0], d.Image)
	return append(args, command[1:]...)
}

func (b *Backend) runDocker(ctx context.Context, t *task, r harness.CommandRequest) execution {
	name := "deerflow-go-" + t.snapshot.ID
	t.mu.Lock()
	t.snapshot.ResourceID = name
	t.mu.Unlock()
	createStarted := false
	created := runProcess(ctx, processSpec{argv: dockerCreateArgs(b.cfg, b.cwd, name, r), env: environment(nil), cwd: b.cwd, started: func(int) { createStarted = true }}, newCapture(4096), newCapture(4096))
	if created.err != nil || created.exitCode == nil || *created.exitCode != 0 {
		if !createStarted {
			return created
		}
		confirmed := b.removeContainer(name)
		if ctx.Err() != nil {
			confirmed = false
		} // a cancelled create RPC may finish later at the daemon
		err := created.err
		if err == nil {
			err = fmt.Errorf("Docker container creation failed")
		}
		return execution{confirmed: confirmed, state: stateFor(err, nil, confirmed), err: err}
	}
	spec := b.processSpec(t, []string{b.cfg.Docker.Executable, "start", "--attach", name}, false)
	spec.env = environment(nil)
	result := runProcess(ctx, spec, t.stdout, t.stderr)
	// A killed attach client does not provide the guest command's exit code.
	if ctx.Err() != nil || result.err != nil {
		result.exitCode = nil
	}
	if !b.removeContainer(name) {
		result.confirmed = false
		result.state = harness.CommandUncertain
		result.err = errors.Join(result.err, harness.ErrCommandUncertain)
	}
	return result
}

func (b *Backend) removeContainer(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if _, err := b.control(ctx, []string{b.cfg.Docker.Executable, "rm", "--force", "--volumes", name}); err == nil {
		return true
	}
	// A successful list with an empty exact-name filter distinguishes an
	// already removed container from an unavailable Docker daemon.
	out, err := b.control(ctx, []string{b.cfg.Docker.Executable, "container", "ls", "--all", "--filter", "name=^/" + name + "$", "--format", "{{.ID}}"})
	return err == nil && strings.TrimSpace(out) == ""
}

func (b *Backend) wslArgs(args ...string) []string {
	result := []string{b.cfg.WSL.Executable, "--distribution", b.cfg.WSL.Distribution}
	if b.cfg.WSL.User != "" {
		result = append(result, "--user", b.cfg.WSL.User)
	}
	return append(append(result, "--exec"), args...)
}
func (b *Backend) runWSL(ctx context.Context, t *task, r harness.CommandRequest) execution {
	argv, _ := json.Marshal(remoteCommand(b.cfg, r))
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	for k, v := range b.cfg.Environment {
		env[k] = v
	}
	environmentJSON, _ := json.Marshal(env)
	marker := "__DEERFLOW_GO_COMMAND_" + t.snapshot.ID + "__"
	args := b.wslArgs("/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "python3", "-u", "-c", linuxSupervisor, b.guestCWD, strconv.FormatFloat(r.Timeout.Seconds(), 'f', 3, 64), marker, string(argv), string(environmentJSON))
	spec := b.processSpec(t, args, true)
	spec.env = environment(nil)
	result := runProcess(ctx, spec, t.stdout, t.stderr)
	if t.view().PID == 0 {
		return result
	}
	return applyWSLReceipt(result, t.stderr, marker)
}

func applyWSLReceipt(result execution, stderr *capture, marker string) execution {
	tail := stderr.tailText()
	at := strings.LastIndex(tail, marker)
	var receipt struct {
		State     harness.CommandState `json:"state"`
		ExitCode  *int                 `json:"exitCode"`
		Confirmed bool                 `json:"confirmed"`
	}
	if at < 0 || json.Unmarshal([]byte(strings.TrimSpace(tail[at+len(marker):])), &receipt) != nil || !receipt.Confirmed || !contains([]string{"completed", "failed", "cancelled", "timed_out"}, string(receipt.State)) {
		return execution{confirmed: false, state: harness.CommandUncertain, err: harness.ErrCommandUncertain}
	}
	stderr.stripSuffix(marker)
	result.exitCode = receipt.ExitCode
	if !result.confirmed {
		return result
	}
	result.state = receipt.State
	result.confirmed = receipt.Confirmed
	result.err = nil
	if receipt.State == harness.CommandCancelled {
		result.err = context.Canceled
	}
	if receipt.State == harness.CommandTimedOut {
		result.err = context.DeadlineExceeded
	}
	return result
}

// HostPath maps a provider absolute workspace path to the fixed client path.
// It deliberately rejects arbitrary container/WSL paths rather than rewriting
// command output or treating all remote filesystem paths as host artifacts.
func (b *Backend) HostPath(remote string) (string, error) {
	base := "/workspace"
	if b.cfg.Provider == harness.SandboxWSL2 {
		base = b.guestCWD
	}
	if b.cfg.Provider != harness.SandboxDocker && b.cfg.Provider != harness.SandboxWSL2 {
		return "", fmt.Errorf("path mapping is only defined for remote command providers")
	}
	if remote != base && !strings.HasPrefix(remote, base+"/") {
		return "", fmt.Errorf("path is outside provider workspace")
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(remote, base), "/")
	for _, part := range strings.Split(rel, "/") {
		if part == ".." || strings.ContainsAny(part, "\\:\x00") {
			return "", fmt.Errorf("invalid provider workspace path")
		}
	}
	return filepath.Join(b.cwd, filepath.FromSlash(rel)), nil
}
