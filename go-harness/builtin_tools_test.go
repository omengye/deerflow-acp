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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

// The child executable acts as a tiny CLI fixture. Production launcher,
// sandbox, Eino middleware and durable permission/receipt handling remain real.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "web" && os.Args[2] == "fixture" {
		if err := os.WriteFile("opencli-effect", []byte("executed"), 0600); err != nil {
			os.Exit(3)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"argv": os.Args[3:], "secret": os.Getenv("TEST_PRIVATE_MODEL_KEY")})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestGoOpenCLIPermissionAndVisibility(t *testing.T) {
	t.Setenv("OPENCLI_BIN", "")
	t.Setenv("TEST_PRIVATE_MODEL_KEY", "never-inherit")
	for _, scenario := range []string{"allow", "deny", "disabled", "denied_by_host", "plan", "read_only"} {
		t.Run(scenario, func(t *testing.T) {
			exe, _ := os.Executable()
			var calls atomic.Int32
			var offered atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools []struct {
						Function struct{ Name string } `json:"function"`
					}
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					w.WriteHeader(400)
					return
				}
				available := false
				for _, item := range body.Tools {
					available = available || item.Function.Name == "host_opencli"
				}
				offered.Store(available || offered.Load())
				delta := map[string]any{"role": "assistant", "content": "finished"}
				finish := "stop"
				if calls.Add(1) == 1 && available {
					delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "opencli-call", "type": "function", "function": map[string]any{"name": "host_opencli", "arguments": `{"description":"test local fixture","site":"web","command":"fixture","arguments":["literal & $(NO_EXEC) %PATH%"]}`}}}}
					finish = "tool_calls"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				chunk, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
			}))
			defer provider.Close()
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", DisableSubagents: true, HostToolsAllowed: scenario != "disabled", BuiltinTools: []harness.BuiltinToolConfig{{Name: "host_opencli", Executable: exe, AllowedSites: []string{"web"}}}, PermissionMode: harness.PermissionModeDangerous}
			if scenario == "denied_by_host" {
				cfg.ToolPolicy.Denylist = []string{"host_opencli"}
			}
			client, err := Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "plan" {
				if err := client.SetMode(context.Background(), session.ID, "plan"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "read_only" {
				if _, err := client.SetConfigOption(context.Background(), session.ID, "approval", harness.ApprovalReadOnly); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(session.CWD, "opencli-effect")
			approvals := 0
			var receipt string
			_, err = client.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "invoke local OpenCLI fixture"}}, func(_ context.Context, event harness.RunEvent) error {
				if event.ToolName == "host_opencli" && event.Kind == "tool_end" && len(event.Content) > 0 {
					receipt = event.Content[0].Text
				}
				return nil
			}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				if request.ToolName != "host_opencli" {
					t.Error("wrong permission target")
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Error("process ran before permission")
				}
				if scenario == "deny" {
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			shouldOffer := scenario == "allow" || scenario == "deny"
			if offered.Load() != shouldOffer || (approvals == 1) != shouldOffer {
				t.Fatalf("offered=%v approvals=%d", offered.Load(), approvals)
			}
			_, statErr := os.Stat(marker)
			if scenario == "allow" {
				if statErr != nil || !strings.Contains(receipt, `"exit_code":0`) || strings.Contains(receipt, "never-inherit") {
					t.Fatalf("missing receipt or credential leak: %s %v", receipt, statErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("rejected/hidden tool executed")
			}
		})
	}
}

func TestBuiltinPolicyRejectsChangedConfigAndDoesNotPersistSecrets(t *testing.T) {
	workspace, cfg := skillFixture(t)
	cfg.BuiltinTools = []harness.BuiltinToolConfig{{Name: "web_search", APIKey: "private-key", HTTPSProxy: "http://user:password@127.0.0.1:8888", MaxResults: 5}}
	db, err := sqlite.Open(filepath.Join(cfg.DataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := skills.NewRegistry(context.Background(), cfg.Skills, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	manager, err := mcp.New(harness.MCPPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	req := harness.RunRequest{Session: harness.Session{ID: "s", CWD: workspace, Mode: "plan"}}
	first, err := extensionFactory(cfg, manager, registry, nil)(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	if strings.Contains(string(first.State), "private-key") || strings.Contains(string(first.State), "password") {
		t.Fatal("secret persisted in checkpoint")
	}
	cfg.BuiltinTools[0].MaxResults = 1
	if _, err := extensionFactory(cfg, manager, registry, nil)(context.Background(), req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("changed tool resources accepted", err)
	}
}
