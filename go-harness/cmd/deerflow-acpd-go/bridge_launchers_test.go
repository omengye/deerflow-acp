package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestExistingRustBridgeGoLaunchScripts(t *testing.T) {
	bridge, daemon := os.Getenv("DEERFLOW_TEST_BRIDGE"), os.Getenv("DEERFLOW_TEST_GO_DAEMON")
	if testing.Short() || bridge == "" || daemon == "" {
		t.Skip("set DEERFLOW_TEST_BRIDGE and DEERFLOW_TEST_GO_DAEMON to built binaries")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("bash")
	if runtime.GOOS == "windows" {
		shell = filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		_, err = os.Stat(shell)
	}
	if err != nil {
		t.Fatal("launcher shell unavailable: ", err)
	}
	fixture := t.TempDir()
	config, ipc := filepath.Join(fixture, "config.yaml"), filepath.Join(fixture, "ipc")
	if err := os.WriteFile(config, []byte("config_version: eino-harness-v1\nsubagents:\n  enabled: false\nmodels:\n  - name: fixture\n    provider: openai\n    model: fixture-model\n    api_key: fixture-only-key\n    base_url: http://127.0.0.1:1/v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "DEERFLOW_GO_DATA_DIR="+filepath.Join(fixture, "state"), "DEER_FLOW_ACP_DAEMON_START_TIMEOUT_MS=15000")
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join(ipc, localhost.EndpointFilename)); os.IsNotExist(err) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bridge, "--runtime-dir", ipc, "--stop-daemon")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("launcher fixture cleanup: %v %s", err, out)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	invoke := func(action string, wantSuccess bool) []byte {
		t.Helper()
		var args []string
		if runtime.GOOS == "windows" {
			script := "acp-service.ps1"
			opts := []string{"-Action", action}
			switch action {
			case "start", "restart":
				script = "acp-daemon.ps1"
			case "stop":
				script, opts = "stop.ps1", nil
			case "removed-python":
				script, opts = "acp-daemon.ps1", []string{"-PythonPath", "obsolete-fixture"}
			case "removed-port":
				script, opts = "stop.ps1", []string{"-Port", "1"}
			}
			args = append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(root, script)}, opts...)
			args = append(args, "-ConfigPath", config, "-BridgePath", bridge, "-DaemonPath", daemon, "-RuntimeDir", ipc)
		} else {
			script, opts := "daemon.sh", []string{action}
			switch action {
			case "start", "stop":
				script, opts = action+".sh", nil
			case "removed-python":
				opts = []string{"start", "--python", "obsolete-fixture"}
			}
			args = append([]string{filepath.Join(root, script)}, opts...)
			args = append(args, "--config", config, "--bridge", bridge, "--daemon", daemon, "--runtime-dir", ipc)
		}
		cmd := exec.CommandContext(ctx, shell, args...)
		cmd.Dir, cmd.Env = fixture, environment
		// Inherited daemon pipe handles must fail this test promptly, allowing
		// cleanup to stop the owned fixture instead of hanging until test panic.
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("launcher %s: %v %s", action, err, out)
		}
		return out
	}
	endpoint := func() localhost.Endpoint {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(ipc, localhost.EndpointFilename))
		var ep localhost.Endpoint
		if err != nil || json.Unmarshal(data, &ep) != nil || ep.Token == "" || ep.BuildID == "" || ep.InstanceID == "" {
			t.Fatal("launcher did not create an authenticated endpoint")
		}
		return ep
	}
	invoke("start", true)
	first := endpoint()
	if out := invoke("status", true); !bytes.Contains(out, []byte("build="+first.BuildID)) || bytes.Contains(out, []byte(first.Token)) {
		t.Fatal("launcher status did not identify Go or leaked endpoint credentials")
	}
	invoke("restart", true)
	if next := endpoint(); next.Token == first.Token {
		t.Fatal("restart reused the old endpoint")
	}
	invoke("stop", true)
	invoke("stop", true) // Idempotent stop must work with Windows PowerShell 5.1 too.
	invoke("status", false)
	if out := invoke("removed-python", false); !strings.Contains(string(out), "has been removed") {
		t.Fatalf("missing migration diagnostic: %s", out)
	}
	if runtime.GOOS == "windows" {
		if out := invoke("removed-port", false); !strings.Contains(string(out), "has been removed") {
			t.Fatalf("missing port migration diagnostic: %s", out)
		}
	}
	if _, err := os.Stat(filepath.Join(ipc, localhost.EndpointFilename)); !os.IsNotExist(err) {
		t.Fatal("stopped daemon still has an endpoint")
	}
}
