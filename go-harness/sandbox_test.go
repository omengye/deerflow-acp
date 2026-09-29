package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKCommandProcessFixture(t *testing.T) {
	if os.Getenv("DEERFLOW_SDK_COMMAND_FIXTURE") != "1" {
		return
	}
	var mode string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if err := os.WriteFile("command-effect", []byte("started"), 0600); err != nil {
		os.Exit(3)
	}
	switch mode {
	case "ok":
		fmt.Fprint(os.Stdout, "command completed")
		os.Exit(0)
	case "nonzero":
		fmt.Fprint(os.Stdout, "partial work")
		fmt.Fprint(os.Stderr, "command failed")
		os.Exit(7)
	case "sleep":
		for i := 0; ; i++ {
			_ = os.WriteFile("heartbeat", []byte(strconv.Itoa(i)), 0600)
			time.Sleep(10 * time.Millisecond)
		}
	default:
		os.Exit(2)
	}
}

// The SDK constructs its production tool factory and real OpenAI adapter. The
// fixture only replaces the provider network endpoint, not permissions/tools.
func commandProvider(t *testing.T, arguments string) (*httptest.Server, func() bool) {
	t.Helper()
	var mu sync.Mutex
	var calls int
	var offered bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad model request", 400)
			return
		}
		mu.Lock()
		calls++
		call := calls
		available := false
		for _, item := range body.Tools {
			available = available || item.Function.Name == "execute"
		}
		offered = offered || available
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{"role": "assistant", "content": "finished"}
		finish := "stop"
		if call == 1 && available {
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "command-call", "type": "function", "function": map[string]any{"name": "execute", "arguments": arguments}}}}
			finish = "tool_calls"
		}
		chunk := map[string]any{"id": "command-fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(server.Close)
	return server, func() bool { mu.Lock(); defer mu.Unlock(); return offered }
}

func commandSDK(t *testing.T, mode string, enabled bool) (*Client, harness.Session, func() bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{"executable": executable, "args": []string{"-test.run=^TestSDKCommandProcessFixture$", "--", mode}})
	provider, offered := commandProvider(t, string(arguments))
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", BaseURL: provider.URL + "/v1", APIKey: "fixture-only", DisableSubagents: true,
		Sandbox: harness.SandboxConfig{Enabled: enabled, Provider: harness.SandboxLocal, AllowedExecutables: []string{executable}, Environment: map[string]string{"DEERFLOW_SDK_COMMAND_FIXTURE": "1"}, Limits: harness.CommandLimits{Timeout: 15 * time.Second}}}
	client, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	session, err := client.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return client, session, offered
}

func TestSDKCommandPermissionPrecedesProcessAndReceipt(t *testing.T) {
	for _, scenario := range []string{"allow", "deny", "nonzero", "disabled", "read_only", "plan"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "ok"
			if scenario == "nonzero" {
				mode = "nonzero"
			}
			client, session, offered := commandSDK(t, mode, scenario != "disabled")
			if scenario == "read_only" {
				if _, err := client.SetConfigOption(context.Background(), session.ID, "approval", harness.ApprovalReadOnly); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "plan" {
				if err := client.SetMode(context.Background(), session.ID, "plan"); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(session.CWD, "command-effect")
			var approvals int
			var executionEvent bool
			var end *harness.RunEvent
			_, err := client.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "execute fixture"}}, func(_ context.Context, event harness.RunEvent) error {
				if event.ToolName != "execute" {
					return nil
				}
				if event.Kind == "tool_execute" {
					executionEvent = true
					if approvals != 1 {
						t.Error("execution receipt preceded approval")
					}
					if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Error("process started before execution receipt")
					}
				}
				if event.Kind == "tool_end" {
					copy := event
					end = &copy
				}
				return nil
			}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				if request.ToolName != "execute" || request.ToolCallID != "command-call" {
					t.Errorf("unexpected permission: %+v", request)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Error("process started before permission")
				}
				if scenario == "deny" {
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if scenario == "nonzero" {
				if err == nil {
					t.Error("nonzero command was reported successful")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			shouldOffer := scenario == "allow" || scenario == "deny" || scenario == "nonzero"
			shouldRun := scenario == "allow" || scenario == "nonzero"
			if offered() != shouldOffer || (approvals == 1) != shouldOffer || executionEvent != shouldRun {
				t.Fatalf("offered=%v approvals=%d execute=%v", offered(), approvals, executionEvent)
			}
			_, statErr := os.Stat(marker)
			if shouldRun && statErr != nil || !shouldRun && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unexpected process side effect: %v", statErr)
			}
			if shouldRun {
				if end == nil {
					t.Fatal("missing terminal receipt")
				}
				wantStatus := "completed"
				if scenario == "nonzero" {
					wantStatus = "failed"
				}
				if end.Status != wantStatus || len(end.Content) == 0 {
					t.Fatalf("terminal=%+v", end)
				}
				var snapshot harness.CommandSnapshot
				if err := json.Unmarshal([]byte(end.Content[0].Text), &snapshot); err != nil {
					t.Fatal(err)
				}
				if !snapshot.TerminationConfirmed || snapshot.ExitCode == nil {
					t.Fatalf("missing process completion: %+v", snapshot)
				}
				if scenario == "nonzero" && (*snapshot.ExitCode != 7 || snapshot.Stdout.Text != "partial work") {
					t.Fatalf("lost failure output: %+v", snapshot)
				}
			}
		})
	}
}

func TestSDKCommandCancellationJoinsRealProcess(t *testing.T) {
	client, session, _ := commandSDK(t, "sleep", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var terminal string
	go func() {
		_, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "execute sleep"}}, func(_ context.Context, event harness.RunEvent) error {
			if event.ToolName == "execute" && event.Kind == "tool_end" && len(event.Content) > 0 {
				terminal = event.Content[0].Text
			}
			return nil
		}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.AllowOnce, nil
		})
		done <- err
	}()
	heartbeat := filepath.Join(session.CWD, "heartbeat")
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	started := false
	for !started {
		select {
		case <-tick.C:
			_, err := os.Stat(heartbeat)
			started = err == nil
		case err := <-done:
			t.Fatalf("run returned before command started: %v", err)
		case <-deadline.C:
			t.Fatal("command did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancellation did not join process")
	}
	var snapshot harness.CommandSnapshot
	if err := json.Unmarshal([]byte(terminal), &snapshot); err != nil {
		t.Fatalf("terminal %q: %v", terminal, err)
	}
	if !snapshot.TerminationConfirmed || snapshot.State != harness.CommandCancelled {
		t.Fatalf("unjoined cancellation: %+v", snapshot)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(before)) != strings.TrimSpace(string(after)) {
		t.Fatal("process continued after SDK run returned")
	}
}
