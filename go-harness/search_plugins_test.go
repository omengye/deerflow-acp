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
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

func TestApprovedSearchWithChangedSnapshotEndsRun(t *testing.T) {
	ctx := context.Background()
	var searches, modelCalls atomic.Int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		fmt.Fprint(w, `{"results":[{"title":"Found","url":"https://example.test","content":"Fixture"}]}`)
	}))
	defer search.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid test request", http.StatusBadRequest)
			return
		}
		switch modelCalls.Add(1) {
		case 1:
			skillToolResponse(w, request.Stream, "web_search", `{"query":"first"}`, "", "first-search")
		case 2:
			skillToolResponse(w, request.Stream, "web_search", `{"query":"second"}`, "", "second-search")
		case 3:
			skillToolResponse(w, request.Stream, "web_search", `{"query":"new turn"}`, "", "new-search")
		default:
			skillToolResponse(w, request.Stream, "", "", "Follow-up complete")
		}
	}))
	defer model.Close()
	dir := t.TempDir()
	settings := filepath.Join(dir, "search-providers.json")
	client, err := Open(ctx, Config{DataDir: dir, SearchSettingsPath: settings, Provider: "openai", Model: "fixture",
		APIKey: "fixture", BaseURL: model.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeAll,
		BuiltinTools: []harness.BuiltinToolConfig{{Name: "web_search"}},
		Search:       harness.SearchConfig{DefaultProvider: "searxng", Providers: map[string]harness.SearchProviderConfig{"searxng": {BaseURL: search.URL}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := client.search.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	result, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "search twice"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals++
		if p.ToolCallID == "second-search" {
			path := filepath.Join(settings+".versions", pin.Ref.Providers["searxng"]+".json")
			if err := os.WriteFile(path, []byte(`{"api_key":"private-fixture"}`), 0600); err != nil {
				return "", err
			}
		}
		return harness.AllowOnce, nil
	})
	if !errors.Is(err, harness.ErrPinnedSearchConfigInvalid) || strings.Contains(err.Error(), "private-fixture") {
		t.Fatal("expected a credential-free snapshot error", err)
	}
	state := result.Execution
	if state == nil || state.Status != harness.ExecutionFailed || state.Resumable || state.BlockedReason != harness.ErrPinnedSearchConfigInvalid.Error() || len(state.WaitingInputs) != 0 {
		t.Fatalf("failed execution = %+v", state)
	}
	var stopReason string
	if err := client.store.DB().QueryRowContext(ctx, "SELECT stop_reason FROM harness_runs WHERE id=?", state.RunID).Scan(&stopReason); err != nil || stopReason != "failed" || result.StopReason != stopReason {
		t.Fatalf("stored stop reason=%q returned=%q err=%v", stopReason, result.StopReason, err)
	}
	if approvals != 2 || searches.Load() != 1 || modelCalls.Load() != 2 {
		t.Fatalf("approvals=%d searches=%d model calls=%d", approvals, searches.Load(), modelCalls.Load())
	}
	receipts, err := client.ListToolReceipts(ctx, session.ID)
	if err != nil || len(receipts) != 2 {
		t.Fatal("search receipts", receipts, err)
	}
	want := map[string]harness.ReceiptState{"first-search": harness.ReceiptCompleted, "second-search": harness.ReceiptNotExecuted}
	for _, receipt := range receipts {
		if receipt.State != want[receipt.ToolCallID] {
			t.Errorf("receipt %s = %s", receipt.ToolCallID, receipt.State)
		}
	}
	_, err = client.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, nil)
	if !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("terminal run accepted another resume", err)
	}
	followup, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "continue with a new search"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if p.ToolCallID != "new-search" {
			t.Errorf("fresh turn requested old approval: %s", p.ToolCallID)
		}
		return harness.AllowOnce, nil
	})
	if err != nil || followup.Execution == nil || followup.Execution.Status != harness.ExecutionCompleted {
		t.Fatal("fresh turn blocked after failed approval", followup, err)
	}
	if searches.Load() != 2 {
		t.Fatalf("fresh search did not use the current configuration: searches=%d", searches.Load())
	}
}

// Exercise one live SDK/Eino runtime and one session across all three searches.
// Only configuration changes between turns; the model adapter, runtime and
// storage stay alive. HTTP fixtures verify the provider that really executed.
func TestSearchPluginsSwitchInLiveRuntimeWithoutRestart(t *testing.T) {
	ctx := context.Background()
	var searches atomic.Int32
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		switch r.URL.Path {
		case "/res/v1/web/search":
			if r.Header.Get("X-Subscription-Token") != "brave-fixture" {
				t.Error("wrong Brave credential")
			}
			fmt.Fprint(w, `{"web":{"results":[{"title":"Brave result","url":"https://example.test/brave","description":"verified source"}]}}`)
		case "/v1/tools/search":
			if r.Header.Get("Authorization") != "Bearer deepseek-fixture" {
				t.Error("wrong DeepSeek credential")
			}
			fmt.Fprint(w, `{"results":[{"title":"DeepSeek result","url":"https://example.test/deepseek","summary":"verified source"}]}`)
		case "/search":
			if r.Header.Get("Authorization") != "" || r.URL.Query().Get("format") != "json" {
				t.Error("SearXNG optional-key contract")
			}
			fmt.Fprint(w, `{"results":[{"title":"SearXNG result","url":"https://example.test/searxng","content":"verified source"}]}`)
		default:
			t.Error("unexpected provider endpoint")
			w.WriteHeader(404)
		}
	}))
	defer searchServer.Close()
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) == 0 {
			w.WriteHeader(400)
			return
		}
		delta := map[string]any{"role": "assistant", "content": "done"}
		finish := "stop"
		if body.Messages[len(body.Messages)-1].Role != "tool" {
			toolName, args := "web_search", `{"query":"fixture search"}`
			message := string(body.Messages[len(body.Messages)-1].Content)
			if strings.Contains(message, "以后默认") {
				toolName, args = "set_search_provider", `{"provider":"deepseek"}`
			}
			if strings.Contains(message, "这次用") {
				args = `{"query":"fixture search","provider":"deepseek"}`
			}
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("search-%d", modelCalls.Add(1)), "type": "function", "function": map[string]any{"name": toolName, "arguments": args}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer model.Close()
	profile := t.TempDir()
	cfg := Config{DataDir: filepath.Join(profile, "data"), Provider: "openai", Model: "fixture", APIKey: "model-fixture", BaseURL: model.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeDangerous,
		BuiltinTools: []harness.BuiltinToolConfig{{Name: "web_search", MaxResults: 5}}, SearchSettingsPath: filepath.Join(profile, "config", "search-providers.json"),
		Search: harness.SearchConfig{DefaultProvider: "brave", Providers: map[string]harness.SearchProviderConfig{"brave": {BaseURL: searchServer.URL, APIKey: "brave-fixture"}, "deepseek": {BaseURL: searchServer.URL, APIKey: "deepseek-fixture"}, "searxng": {BaseURL: searchServer.URL}}}}
	client, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime := client.service
	configCLI, err := websearch.New(cfg.SearchSettingsPath, cfg.Search)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"brave", "deepseek", "searxng", "brave"} {
		if _, err := configCLI.Select(ctx, name); err != nil {
			t.Fatal(err)
		}
		var output string
		_, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Search using my saved provider"}}, func(_ context.Context, event harness.RunEvent) error {
			if event.Kind == "tool_end" && event.ToolName == "web_search" {
				for _, content := range event.Content {
					output += content.Text
				}
			}
			return nil
		}, nil)
		if err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(output, `"provider":"`+name+`"`) || !strings.Contains(output, "https://example.test/"+name) {
			t.Fatalf("%s was not the provider that executed: %s", name, output)
		}
		if client.service != runtime {
			t.Fatal("runtime was replaced")
		}
	}
	var temporary string
	_, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "这次用 DeepSeek 搜索"}}, func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "tool_end" && event.ToolName == "web_search" {
			for _, content := range event.Content {
				temporary += content.Text
			}
		}
		return nil
	}, nil)
	if err != nil || !strings.Contains(temporary, `"provider":"deepseek"`) {
		t.Fatal("one-time provider override failed", err)
	}
	selected, _ := configCLI.Status()
	if selected.DefaultProvider != "brave" {
		t.Fatal("one-time override changed the saved default")
	}
	if searches.Load() != 5 {
		t.Fatal("unexpected number of actual provider calls", searches.Load())
	}
	var approvalRequests int
	if err := client.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM harness_execution_intents WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)", session.ID).Scan(&approvalRequests); err != nil || approvalRequests != 0 {
		t.Fatalf("ordinary searches created %d approval requests: %v", approvalRequests, err)
	}
	// A model-issued preference tool, triggered by an explicit user request,
	// persists the same default that the desktop selector reads.
	_, err = client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "以后默认用 DeepSeek 搜索"}}, nil, nil)
	if err != nil {
		t.Fatal("default preference tool failed", err)
	}
	status, err := client.ManageLocal(ctx, localhost.ManagementRequest{Operation: "search.status", Fields: map[string]json.RawMessage{"operation": json.RawMessage(`"search.status"`)}})
	if err != nil || status.(websearch.Status).DefaultProvider != "deepseek" {
		t.Fatal("management preference switch", err)
	}
}
