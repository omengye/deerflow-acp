package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func openCLIFailureFixture() (int, bool) {
	if len(os.Args) < 4 || os.Args[1] != "web" || os.Args[2] != "failure-fixture" {
		return 0, false
	}
	file, err := os.OpenFile("opencli-failure-effect", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 3, true
	}
	_, err = file.WriteString("once\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return 3, true
	}
	fmt.Fprintln(os.Stderr, "OpenCLI fixture browser bridge unavailable")
	if os.Args[3] == "timeout" {
		time.Sleep(60 * time.Second)
	}
	if os.Args[3] == "auth_required" {
		return 77, true
	}
	return 69, true
}

// Use the real launcher, process backend, permission checkpoint and ACP model
// adapter. A tool failure must remain a failed receipt, not a failed turn.
func TestOpenCLIFailureKeepsConversationUsable(t *testing.T) {
	t.Setenv("OPENCLI_BIN", "")
	for _, scenario := range []string{"nonzero_exit", "auth_required", "timeout", "missing_launcher", "missing_unused", "missing_plan", "missing_read_only"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			missing := strings.HasPrefix(scenario, "missing_")
			callTool := scenario != "missing_unused" && scenario != "missing_plan" && scenario != "missing_read_only"
			if missing {
				executable = filepath.Join(t.TempDir(), "missing-opencli.exe")
			}
			var calls atomic.Int32
			var sawDiagnostic atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				current := calls.Add(1)
				if callTool && current == 2 {
					for _, message := range body.Messages {
						if message.Role != "tool" || !strings.Contains(message.Content, "Tool failed") {
							continue
						}
						if missing {
							sawDiagnostic.Store(strings.Contains(message.Content, "OpenCLI") && strings.Contains(message.Content, "unavailable"))
						} else {
							sawDiagnostic.Store(strings.Contains(message.Content, "OpenCLI fixture browser bridge unavailable") && strings.Contains(message.Content, "Partial effects") && strings.Contains(message.Content, `"termination_confirmed":true`))
						}
					}
				}
				delta := map[string]any{"role": "assistant", "content": "The browser tool failed; I can explain the problem and continue this conversation."}
				finish := "stop"
				if callTool && current == 1 {
					args, _ := json.Marshal(map[string]any{"site": "web", "command": "failure-fixture", "arguments": []string{scenario}})
					delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "failed-opencli", "type": "function", "function": map[string]any{"name": "host_opencli", "arguments": string(args)}}}}
					finish = "tool_calls"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				chunk, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
			}))
			defer provider.Close()
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", DisableSubagents: true, HostToolsAllowed: true, BuiltinTools: []harness.BuiltinToolConfig{{Name: "host_opencli", Executable: executable, AllowedSites: []string{"web"}, Timeout: 3}}, PermissionMode: harness.PermissionModeDangerous}
			client, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "missing_plan" {
				if err := client.SetMode(ctx, session.ID, "plan"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing_read_only" {
				if _, err := client.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalReadOnly); err != nil {
					t.Fatal(err)
				}
			}
			approvals := 0
			approve := func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				return harness.AllowOnce, nil
			}
			var response strings.Builder
			result, runErr := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "try the browser tool, then explain any failure"}}, func(_ context.Context, event harness.RunEvent) error {
				if event.Kind == "text_delta" {
					response.WriteString(event.Text)
				}
				return nil
			}, approve)
			if runErr != nil || result.StopReason != "end_turn" || !strings.Contains(response.String(), "continue this conversation") {
				t.Fatalf("OpenCLI failure stopped the turn: result=%+v err=%v response=%q", result, runErr, response.String())
			}
			if callTool && (!sawDiagnostic.Load() || approvals != 1) {
				t.Fatalf("failure did not return to model through approved tool call: diagnostic=%t approvals=%d", sawDiagnostic.Load(), approvals)
			}
			if !callTool && approvals != 0 {
				t.Fatal("unused or hidden OpenCLI requested approval")
			}
			receipts, err := client.ListToolReceipts(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if callTool {
				want := harness.ReceiptFailed
				if missing {
					want = harness.ReceiptNotExecuted
				}
				if len(receipts) != 1 || receipts[0].State != want || receipts[0].Error == "" {
					t.Fatalf("failed tool was hidden or stranded: %+v", receipts)
				}
			}
			followup, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "continue without the browser"}}, nil, approve)
			if err != nil || followup.StopReason != "end_turn" {
				t.Fatalf("next prompt failed: result=%+v err=%v", followup, err)
			}
			if !missing {
				body, err := os.ReadFile(filepath.Join(session.CWD, "opencli-failure-effect"))
				if err != nil || string(body) != "once\n" {
					t.Fatalf("failed tool effects were replayed or rolled back: %q %v", body, err)
				}
			}
		})
	}
}
