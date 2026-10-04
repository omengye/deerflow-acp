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

// Exercise the shipped Bridge's discovery and actual Go daemon main entry,
// including detached startup and shutdown. No interpreter fixture is executed.
func TestExistingRustBridgeGoDaemonDiscovery(t *testing.T) {
	bridge, daemon := os.Getenv("DEERFLOW_TEST_BRIDGE"), os.Getenv("DEERFLOW_TEST_GO_DAEMON")
	if testing.Short() || bridge == "" || daemon == "" {
		t.Skip("set DEERFLOW_TEST_BRIDGE and DEERFLOW_TEST_GO_DAEMON to built binaries")
	}
	root := t.TempDir()
	suffix, venvBin, interpreter := "", "bin", "python3"
	if runtime.GOOS == "windows" {
		suffix, venvBin, interpreter = ".exe", "Scripts", "python.exe"
	}
	bundle := filepath.Join(root, "bundle with spaces")
	bridgeCopy := filepath.Join(bundle, "deerflow-acp"+suffix)
	sibling := filepath.Join(bundle, "deerflow-acpd"+suffix)
	explicit := filepath.Join(root, "go bin", "custom-daemon"+suffix)
	copyBridgeFixture(t, bridge, bridgeCopy)
	copyBridgeFixture(t, daemon, sibling)
	copyBridgeFixture(t, daemon, explicit)
	pathDir := filepath.Join(root, "old PATH")
	for _, path := range []string{
		filepath.Join(root, ".venv", venvBin, interpreter),
		filepath.Join(bundle, "runtime", interpreter),
		filepath.Join(bundle, "runtime", "bin", interpreter),
		filepath.Join(bundle, interpreter),
		filepath.Join(pathDir, interpreter),
		filepath.Join(pathDir, "deerflow-acpd"+suffix),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("inert legacy-discovery fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"sibling", "explicit", "environment"} {
		t.Run(mode, func(t *testing.T) {
			configDir := t.TempDir()
			config := filepath.Join(configDir, "config.yaml")
			if err := os.WriteFile(config, []byte("config_version: eino-harness-v1\nsubagents:\n  enabled: false\nmodels:\n  - name: fixture\n    provider: openai\n    model: fixture-model\n    api_key: fixture-only-key\n    base_url: http://127.0.0.1:1/v1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ipc := filepath.Join(t.TempDir(), "ipc")
			args := []string{"--runtime-dir", ipc, "--config", config}
			var environment []string
			for _, item := range os.Environ() {
				key, _, _ := strings.Cut(strings.ToUpper(item), "=")
				if !strings.HasPrefix(key, "DEERFLOW_") && !strings.HasPrefix(key, "DEER_FLOW_") && key != "PATH" {
					environment = append(environment, item)
				}
			}
			environment = append(environment,
				"PATH="+pathDir,
				"DEER_FLOW_ACP_PYTHON="+filepath.Join(pathDir, interpreter),
				"DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS=15000",
				"DEERFLOW_GO_DATA_DIR="+filepath.Join(configDir, "go-state"),
			)
			relative, err := filepath.Rel(root, explicit)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "explicit":
				args = append(args, "--daemon", relative)
				environment = append(environment, "DEER_FLOW_ACP_DAEMON="+filepath.Join(root, "missing-override"))
			case "environment":
				if err := os.Remove(sibling); err != nil {
					t.Fatal(err)
				}
				environment = append(environment, "DEER_FLOW_ACP_DAEMON="+relative)
			}
			command := func(ctx context.Context, options ...string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, bridgeCopy, append(append([]string{}, args...), options...)...)
				cmd.Dir, cmd.Env = root, environment
				cmd.WaitDelay = 3 * time.Second
				return cmd
			}
			t.Cleanup(func() {
				if _, err := os.Stat(filepath.Join(ipc, localhost.EndpointFilename)); os.IsNotExist(err) {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				out, err := command(ctx, "--stop-daemon").CombinedOutput()
				if err != nil {
					t.Errorf("fixture daemon cleanup: %v %s", err, out)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			invoke := func(mode, input string) []byte {
				t.Helper()
				cmd := command(ctx, mode)
				cmd.Stdin = strings.NewReader(input)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("Bridge %s: %v %s", mode, err, out)
				}
				return out
			}
			if out := invoke("--start-daemon", ""); !bytes.Contains(out, []byte("build=deerflow-go")) {
				t.Fatalf("not the Go daemon: %s", out)
			}
			data, err := os.ReadFile(filepath.Join(ipc, localhost.EndpointFilename))
			var ep localhost.Endpoint
			if err != nil || json.Unmarshal(data, &ep) != nil || ep.Token == "" {
				t.Fatal("missing daemon endpoint identity")
			}
			if out := invoke("--status", ""); !bytes.Contains(out, []byte("build=deerflow-go")) || bytes.Contains(out, []byte(ep.Token)) {
				t.Fatal("invalid or credential-bearing Bridge status")
			}
			if out := invoke("--manage", `{"operation":"daemon.status"}`); !bytes.Contains(out, []byte(`"config_revision"`)) || bytes.Contains(out, []byte(ep.Token)) {
				t.Fatal("missing config identity or credential-bearing management response")
			}
			proxy := command(ctx, "--no-auto-start")
			in, err := proxy.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := proxy.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			proxy.Stderr = &diagnostics
			if err := proxy.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = in.Close()
				if proxy.ProcessState == nil {
					_ = proxy.Process.Kill()
					_ = proxy.Wait()
				}
			}()
			acp := &rpcClient{in: in, out: json.NewDecoder(out)}
			acp.success(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
			acp.success(t, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
			_ = in.Close()
			if err := proxy.Wait(); err != nil {
				t.Fatalf("ACP proxy: %v %s", err, diagnostics.String())
			}
			if out := invoke("--stop-daemon", ""); !bytes.Contains(out, []byte("stopped")) {
				t.Fatalf("stop: %s", out)
			}
			if _, err := os.Stat(filepath.Join(ipc, localhost.EndpointFilename)); !os.IsNotExist(err) {
				t.Fatal("daemon endpoint was not removed after STOP")
			}
		})
	}
}

func copyBridgeFixture(t *testing.T, source, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy fixture executable: %v %v", copyErr, closeErr)
	}
}
