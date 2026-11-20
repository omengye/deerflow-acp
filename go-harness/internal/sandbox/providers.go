package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path"
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
		return b.probeWSL(ctx)
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
	if out.snapshot(false).Truncated {
		return "", fmt.Errorf("provider control response exceeds limit")
	}
	return out.snapshot(false).Text, nil
}

func (b *Backend) run(ctx context.Context, t *task, r harness.CommandRequest) execution {
	if err := b.checkWorkspace(); err != nil {
		return execution{confirmed: true, notExecuted: true, state: harness.CommandFailed, err: err}
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
		spec := b.processSpec(t, argv, false)
		if s := r.Secrets; s != nil {
			spec.env = environment(withSecrets(b.cfg.Environment, s.Environment(s.FilesDir, s.OutDir, filepath.Join)))
		}
		if b.cfg.Provider == harness.SandboxLocal {
			// Launch the validated canonical binary, retaining the invocation
			// name that interpreters use to discover a virtual environment.
			spec.argv0 = t.argv0
		}
		return runProcess(ctx, spec, t.stdout, t.stderr)
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

func bindMount(source, target string, readOnly bool) string {
	var mount strings.Builder
	writer := csv.NewWriter(&mount)
	fields := []string{"type=bind", "source=" + source, "target=" + target}
	if readOnly {
		fields = append(fields, "readonly")
	}
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(mount.String(), "\n")
}

// Container paths of injected secret material.
const (
	dockerSecretFiles = "/run/secrets"
	dockerSecretOut   = "/run/deerflow-secret-out"
)

// dockerSecretEnvironment is the injected environment as the container sees
// it. The docker CLI receives these values in its own environment and copies
// them by name, so plaintext never appears in its argument vector.
func dockerSecretEnvironment(r harness.CommandRequest) map[string]string {
	return r.Secrets.Environment(dockerSecretFiles, dockerSecretOut, path.Join)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// writeDockerSecretEnvFile writes single-line injected secrets to a private
// 0600 file for `docker create --env-file`, keeping them completely out of the
// host docker CLI's process environment. Multiline values (unsupported by
// --env-file) are returned in fallbackEnv (their names were already validated
// against DOCKER_* and reserved host variables by secrets.ValidEnvName).
func writeDockerSecretEnvFile(injected map[string]string, r harness.CommandRequest) (envFile string, fallbackEnv map[string]string, cleanup func(), err error) {
	cleanup = func() {}
	if len(injected) == 0 {
		return "", nil, cleanup, nil
	}
	var lines strings.Builder
	for _, key := range sortedKeys(injected) {
		val := injected[key]
		if strings.ContainsAny(val, "\r\n") {
			if fallbackEnv == nil {
				fallbackEnv = map[string]string{}
			}
			fallbackEnv[key] = val
			continue
		}
		lines.WriteString(key)
		lines.WriteByte('=')
		lines.WriteString(val)
		lines.WriteByte('\n')
	}
	if lines.Len() == 0 {
		return "", fallbackEnv, cleanup, nil
	}
	dir := ""
	if s := r.Secrets; s != nil {
		if s.FilesDir != "" {
			dir = filepath.Dir(s.FilesDir)
		} else if s.OutDir != "" {
			dir = filepath.Dir(s.OutDir)
		}
	}
	if dir != "" {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, cleanup, err
		}
	}
	file, err := os.CreateTemp(dir, ".docker-env-*")
	if err != nil {
		return "", nil, cleanup, err
	}
	path := file.Name()
	cleanup = func() { _ = os.Remove(path) }
	if err = file.Chmod(0o600); err == nil {
		_, err = file.WriteString(lines.String())
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	return path, fallbackEnv, cleanup, nil
}

func dockerCreateArgs(cfg harness.SandboxConfig, cwd, name string, r harness.CommandRequest) []string {
	return dockerCreateArgsWithEnvFile(cfg, cwd, name, r, "", dockerSecretEnvironment(r))
}

func dockerCreateArgsWithEnvFile(cfg harness.SandboxConfig, cwd, name string, r harness.CommandRequest, envFile string, cliEnv map[string]string) []string {
	d := cfg.Docker
	args := []string{d.Executable, "create", "--name", name, "--pull=never", "--init", "--network", d.Network, "--cpus", strconv.FormatFloat(d.CPUs, 'f', -1, 64), "--memory", strconv.FormatInt(d.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(d.MemoryBytes, 10), "--pids-limit", strconv.Itoa(d.PIDs), "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=67108864", "--user", d.User, "--workdir", "/workspace", "--mount", bindMount(cwd, "/workspace", d.WorkspaceReadOnly)}
	injected := dockerSecretEnvironment(r)
	configured := make(map[string]string, len(cfg.Environment))
	for key, value := range cfg.Environment {
		configured[key] = value
	}
	for key := range injected {
		for existing := range configured {
			if strings.EqualFold(existing, key) {
				delete(configured, existing)
			}
		}
	}
	for _, key := range sortedKeys(configured) {
		args = append(args, "--env", key+"="+configured[key])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	for _, key := range sortedKeys(cliEnv) {
		args = append(args, "--env", key)
	}
	if s := r.Secrets; s != nil {
		if len(s.Files) > 0 {
			args = append(args, "--mount", bindMount(s.FilesDir, dockerSecretFiles, true))
		}
		if s.OutDir != "" {
			args = append(args, "--mount", bindMount(s.OutDir, dockerSecretOut, false))
		}
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
	envFile, fallbackEnv, removeEnvFile, envErr := writeDockerSecretEnvFile(dockerSecretEnvironment(r), r)
	if envErr != nil {
		return execution{confirmed: true, notExecuted: true, state: harness.CommandFailed, err: fmt.Errorf("prepare Docker secret environment: %w", envErr)}
	}
	defer removeEnvFile()
	createStarted := false
	created := runProcess(ctx, processSpec{argv: dockerCreateArgsWithEnvFile(b.cfg, b.cwd, name, r, envFile, fallbackEnv), env: environment(fallbackEnv), cwd: b.cwd, started: func(int) { createStarted = true }}, newCapture(4096), newCapture(4096))
	removeEnvFile()
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
