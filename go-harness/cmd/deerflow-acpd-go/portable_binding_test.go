package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// Run against the actual package binaries, using a disposable profile and no
// model requests. This catches disagreement between Rust and Go path policies.
func TestExistingRustBridgePortableDataBinding(t *testing.T) {
	bridge, daemon, stdio := os.Getenv("DEERFLOW_TEST_BRIDGE"), os.Getenv("DEERFLOW_TEST_GO_DAEMON"), os.Getenv("DEERFLOW_TEST_GO_STDIO")
	if bridge == "" || daemon == "" || stdio == "" {
		t.Skip("set the three packaged executable paths")
	}
	root := t.TempDir()
	bundle := filepath.Join(root, "portable with spaces")
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	bridge = copyPortableBinary(t, bridge, bundle, "deerflow-acp"+suffix)
	daemon = copyPortableBinary(t, daemon, bundle, "deerflow-acpd"+suffix)
	stdio = copyPortableBinary(t, stdio, bundle, "deerflow-acp-go"+suffix)
	data := filepath.Join(bundle, "user-data")
	original := filepath.Join(data, "config", "go-harness-state")
	other := filepath.Join(data, "data", "go-harness")
	ipc := filepath.Join(data, "runtime", "acp-go")
	for _, dir := range []string{original, other} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, value []byte) {
		t.Helper()
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(original, "harness.db"), nil) // SQLite initializes this selected fixture only.
	write(filepath.Join(other, "harness.db"), []byte("another historical store; must never be opened"))
	config := filepath.Join(data, "config", "config.yaml")
	write(config, []byte("config_version: eino-harness-v1\nsubagents:\n  enabled: false\nlocal_acp:\n  session_cleanup_enabled: false\nmodels:\n  - name: fixture\n    provider: openai\n    model: fixture-model\n    api_key: fixture-only-key\n    base_url: http://127.0.0.1:1/v1\n"))
	binding := filepath.Join(data, "config", "runtime-data.json")
	bindingBytes := []byte(`{"data_dir":"config/go-harness-state"}`)
	write(binding, bindingBytes)
	var environment []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(strings.ToUpper(item), "=")
		if !strings.HasPrefix(key, "DEERFLOW_") && !strings.HasPrefix(key, "DEER_FLOW_") {
			environment = append(environment, item)
		}
	}
	environment = append(environment, "DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS=15000")
	command := func(executable string, args ...string) *exec.Cmd {
		cmd := exec.Command(executable, args...)
		cmd.Env, cmd.Dir, cmd.WaitDelay = environment, root, 3*time.Second
		return cmd
	}
	invoke := func(executable string, success bool, extraEnv []string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Env, cmd.Dir, cmd.WaitDelay = append(append([]string{}, environment...), extraEnv...), root, 3*time.Second
		output, err := cmd.CombinedOutput()
		if (err == nil) != success {
			t.Fatalf("%s %v: %v %s", filepath.Base(executable), args, err, output)
		}
		return output
	}
	endpoint := func() localhost.Endpoint {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(ipc, localhost.EndpointFilename))
		var ep localhost.Endpoint
		if err != nil || json.Unmarshal(raw, &ep) != nil || ep.DataDir != original {
			t.Fatalf("wrong endpoint data directory: %s %v", ep.DataDir, err)
		}
		return ep
	}
	repository, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	restart := func(success bool) {
		t.Helper()
		if runtime.GOOS == "windows" {
			shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			invoke(shell, success, nil, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(repository, "acp-daemon.ps1"), "-Action", "restart", "-ConfigPath", config, "-BridgePath", bridge, "-DaemonPath", daemon, "-RuntimeDir", ipc)
		} else {
			invoke("bash", success, nil, filepath.Join(repository, "daemon.sh"), "restart", "--config", config, "--bridge", bridge, "--daemon", daemon, "--runtime-dir", ipc)
		}
	}
	t.Cleanup(func() { _ = command(bridge, "--stop-daemon").Run() })
	invoke(bridge, true, nil, "--start-daemon") // no config/data/runtime environment
	ep := endpoint()
	client := acpClient(t, ep)
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(client.success(t, "session/new", map[string]any{"cwd": root, "mcpServers": []any{}}), &session); err != nil || session.ID == "" {
		t.Fatal("session was not created")
	}
	_ = client.in.(io.Closer).Close()
	invoke(bridge, false, []string{"DEERFLOW_GO_DATA_DIR=" + other}, "--start-daemon")
	invoke(daemon, false, nil, "--check-paths", "--data-dir", other)
	invoke(stdio, false, []string{"DEERFLOW_GO_DATA_DIR=" + other})
	if endpoint().PID != ep.PID {
		t.Fatal("rejected override restarted the daemon")
	}
	var checked map[string]string
	if err := json.Unmarshal(invoke(daemon, true, nil, "--check-paths"), &checked); err != nil || checked["data_dir"] != original || checked["runtime_dir"] != ipc {
		t.Fatalf("native preflight disagrees: %v %v", checked, err)
	}
	// The same config path is insufficient to authorize attaching to an endpoint.
	endpointPath := filepath.Join(ipc, localhost.EndpointFilename)
	raw, err := os.ReadFile(endpointPath)
	if err != nil {
		t.Fatal(err)
	}
	wrong := ep
	wrong.DataDir = other
	wrongBytes, _ := json.Marshal(wrong)
	write(endpointPath, wrongBytes)
	invoke(bridge, false, nil, "--start-daemon")
	invoke(bridge, false, nil, "--status")
	restart(false)
	write(endpointPath, raw)
	invoke(bridge, true, nil, "--status") // rejected restart left the original daemon alive
	if err := os.Remove(binding); err != nil {
		t.Fatal(err)
	}
	invoke(bridge, false, nil, "--start-daemon")
	invoke(daemon, false, nil, "--check-paths")
	restart(false)
	write(binding, bindingBytes)
	restart(true)
	client = acpClient(t, endpoint())
	client.success(t, "session/resume", map[string]any{"sessionId": session.ID, "cwd": root, "mcpServers": []any{}})
	_ = client.in.(io.Closer).Close()
	invoke(bridge, true, nil, "--stop-daemon")
	// Direct daemon startup must discover the same portable config and database.
	p := &daemonProcess{cmd: command(daemon), done: make(chan struct{})}
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	ep = p.endpoint(t, ipc)
	if ep.DataDir != original {
		t.Fatalf("direct daemon switched database: %s", ep.DataDir)
	}
	client = acpClient(t, ep)
	client.success(t, "session/resume", map[string]any{"sessionId": session.ID, "cwd": root, "mcpServers": []any{}})
	_ = client.in.(io.Closer).Close()
	invoke(bridge, true, nil, "--stop-daemon")
	p.wait(t, true)
	// The standalone stdio executable must resume the original ID too.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, stdio)
	cmd.Env, cmd.Dir = environment, root
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &rpcClient{in: in, out: json.NewDecoder(out)}
	c.success(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	c.success(t, "session/resume", map[string]any{"sessionId": session.ID, "cwd": root, "mcpServers": []any{}})
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("stdio exit: %v %s", err, stderr.String())
	}
	untouched, err := os.ReadFile(filepath.Join(other, "harness.db"))
	if err != nil || string(untouched) != "another historical store; must never be opened" {
		t.Fatal("alternate store changed")
	}
}

func copyPortableBinary(t *testing.T, source, directory, name string) string {
	t.Helper()
	target := filepath.Join(directory, name)
	copyBridgeFixture(t, source, target)
	return target
}
