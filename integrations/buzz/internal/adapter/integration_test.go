package adapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in process integration uses a real release Bridge and daemon with an
// isolated localhost model fixture. It never loads the user's model config.
func TestNativeReleaseBuzzIntegration(t *testing.T) {
	bridge, daemon := os.Getenv("DEERFLOW_TEST_BRIDGE"), os.Getenv("DEERFLOW_TEST_GO_DAEMON")
	if bridge == "" || daemon == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_BRIDGE and DEERFLOW_TEST_GO_DAEMON")
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		if bytes.Contains(body, []byte("write-permission")) && !bytes.Contains(body, []byte(`"role":"tool"`)) {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"write-test\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"buzz-result.txt\\\",\\\"content\\\":\\\"approved\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	runtimeDir, dataDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	fixtureConfig := "models:\n  - name: fixture\n    use: langchain_openai:ChatOpenAI\n    model: fixture-model\n    api_key: fixture-only-key\n    base_url: " + provider.URL + "/v1\n"
	if e := os.WriteFile(configPath, []byte(fixtureConfig), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(daemon, "--config", configPath, "--data-dir", dataDir, "--runtime-dir", runtimeDir, "--provider", "openai", "--model", "fixture-model", "--base-url", provider.URL+"/v1", "--max-connections", "4")
	hideCommand(cmd)
	cmd.Env = mergeEnv(os.Environ(), map[string]string{"DEERFLOW_MODEL_API_KEY": "fixture-only-key", "DEERFLOW_GO_LOG_DIR": filepath.Join(runtimeDir, "logs")})
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop := exec.CommandContext(ctx, bridge, "--stop-daemon", "--config", configPath, "--runtime-dir", runtimeDir)
		hideCommand(stop)
		stop.Stdout, stop.Stderr = io.Discard, io.Discard
		_ = stop.Run()
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(runtimeDir, "endpoint.json")); e == nil {
			break
		}
		select {
		case e := <-done:
			t.Fatal("fixture daemon exited", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture daemon startup timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, policy := range []string{"deny", "allow_once"} {
		t.Run(policy, func(t *testing.T) {
			a, _ := offlineApp(t)
			a.Config.Deerflow.Command = bridge
			a.Config.Deerflow.Args = []string{"--protocol", "v2", "--no-auto-start", "--config", configPath, "--runtime-dir", runtimeDir}
			a.Config.Deerflow.PermissionPolicy = policy
			a.Config.Deerflow.Timeout = 10
			a.Config.Deerflow.PromptTimeout = 20
			client := NewACP(a.Config)
			var diagnostics bytes.Buffer
			client.stderr = &diagnostics
			a.Agent = client
			t.Cleanup(client.Close)
			sends := 0
			reactions := 0
			initialModelCalls := calls.Load()
			a.Buzz.runner = func(_ context.Context, args []string, input string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
				if args[0] == "reactions" {
					reactions++
					if calls.Load() != initialModelCalls {
						t.Error("model started before receipt reaction")
					}
					return []byte(`{"accepted":true}`), nil
				}
				sends++
				if policy == "allow_once" && !strings.Contains(input, "Done.") {
					t.Error("missing native model response", input)
				}
				if policy == "deny" && !strings.Contains(input, "permission was declined") {
					t.Error("missing permission denial", input)
				}
				return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
			}
			m := testMessage(t, "a", 1)
			m.Content = "write-permission"
			_ = a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread")
			if e := a.processPending(context.Background()); e != nil {
				t.Fatal(e)
			}
			counts, _ := a.State.Counts()
			if counts["done"] != 1 || sends != 1 || reactions != 1 {
				var detail string
				_ = a.State.db.QueryRow(`SELECT COALESCE(last_error,'') FROM inbox_messages WHERE message_key=?`, m.ID).Scan(&detail)
				t.Fatal(counts, sends, reactions, detail, diagnostics.String())
			}
			_, e := os.Stat(filepath.Join(a.Config.Deerflow.Workspace, "buzz-result.txt"))
			if policy == "allow_once" && e != nil {
				t.Fatal("approved tool did not run", e)
			}
			if policy == "deny" && !os.IsNotExist(e) {
				t.Fatal("denied tool wrote a file", e)
			}
			previous := calls.Load()
			client.Close()
			if e = client.Open(context.Background()); e != nil {
				t.Fatal(e)
			}
			session, e := a.State.Session(m.conversation("thread"), a.Config.Deerflow.Workspace, backendID(a.Config))
			if e != nil || session == "" {
				t.Fatal(session, e)
			}
			if _, e = client.AttachOrCreate(context.Background(), session); e != nil {
				t.Fatal(e)
			}
			if calls.Load() != previous {
				t.Fatal("resume re-executed model")
			}
		})
	}
}
