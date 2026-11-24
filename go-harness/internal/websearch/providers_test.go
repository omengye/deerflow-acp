package websearch

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
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func isolatedEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BRAVE_API_KEY", "DEEPSEEK_API_KEY", "SEARXNG_BASE_URL", "SEARXNG_API_KEY", "SEARXNG_TOKEN"} {
		t.Setenv(name, "")
	}
	t.Setenv("USERPROFILE", t.TempDir())
}

func mustManager(t *testing.T, path string, cfg harness.SearchConfig) *Manager {
	t.Helper()
	m, err := New(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestProvidersHTTPContractsAndUnifiedResults(t *testing.T) {
	isolatedEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/res/v1/web/search":
			if r.Method != "GET" || r.Header.Get("X-Subscription-Token") != "brave-key" || r.URL.Query().Get("freshness") != "pm" || r.URL.Query().Get("count") != "2" {
				t.Error("Brave request contract")
			}
			fmt.Fprint(w, `{"web":{"results":[{"title":"Brave","url":"https://example.test/brave","description":"snippet"}]}}`)
		case "/v1/tools/search":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer deepseek-key" || body["count"] != float64(2) || body["need_content"] != true || !strings.Contains(body["query"].(string), " after:") {
				t.Error("DeepSeek request contract")
			}
			fmt.Fprint(w, `{"results":[{"title":"DeepSeek","url":"https://example.test/deepseek","snippet":"short","summary":"long summary","content":"# Body","rerank_score":2.4,"date":"2026-11-01T00:00:00Z"}]}`)
		case "/search":
			if r.Method != "GET" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("time_range") != "month" || r.Header.Get("Authorization") != "" {
				t.Error("SearXNG without Key contract")
			}
			fmt.Fprint(w, `{"results":[{"title":"low","url":"https://example.test/searxng","content":"low snippet","score":1},{"title":"high","url":"https://example.test/searxng","content":"high snippet","score":3},{"title":"other","url":"https://example.test/other","content":"second","score":2},{"title":"bad","url":"javascript:alert(1)","score":9}]}`)
		case "/v1/tools/fetch":
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer deepseek-key" {
				t.Error("Fetch contract")
			}
			fmt.Fprint(w, `{"title":"Page","content":"# Markdown"}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{"brave": {BaseURL: server.URL, APIKey: "brave-key"}, "deepseek": {BaseURL: server.URL, APIKey: "deepseek-key"}, "searxng": {BaseURL: server.URL}}}
	m := mustManager(t, "", cfg)
	snapshot, err := m.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		out, err := snapshot.Search(context.Background(), Request{Provider: name, Query: "test", TimeRange: "month", MaxResults: 2, NeedContent: true})
		if err != nil {
			t.Fatal(name, err)
		}
		if out.Provider != name || out.TotalResults == 0 || out.Results[0].URL != "https://example.test/"+name {
			t.Fatalf("invalid normalized result for %s", name)
		}
		if name == "searxng" && (out.TotalResults != 2 || out.Results[0].Title != "high") {
			t.Fatal("SearXNG ordering/deduplication")
		}
		if name == "deepseek" && (out.Results[0].Content != "long summary" || out.Results[0].PageContent != "# Body" || out.Results[0].RerankScore == nil) {
			t.Fatal("DeepSeek extension information lost")
		}
	}
	content, err := snapshot.Fetch(context.Background(), "https://example.test/page")
	if err != nil || content != "# Markdown" {
		t.Fatal("DeepSeek fetch", err)
	}
}

func TestProviderLimitsErrorsAndCredentialIsolation(t *testing.T) {
	isolatedEnvironment(t)
	var requests atomic.Int32
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" || r.Header.Get("X-Subscription-Token") != "" {
			leaked.Store(true)
		}
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer origin.Close()
	m := mustManager(t, "", harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{"deepseek": {BaseURL: origin.URL, APIKey: "sensitive"}, "searxng": {BaseURL: origin.URL, APIKey: "sensitive", AuthHeader: "X-API-Key"}}})
	snapshot, _ := m.Snapshot(nil)
	for _, in := range []Request{{Provider: "deepseek", Query: strings.Repeat("中", 129)}, {Provider: "searxng", Query: "test", TimeRange: "week"}, {Provider: "unknown", Query: "test"}, {Provider: "deepseek", Query: " "}} {
		if _, err := snapshot.Search(context.Background(), in); err == nil {
			t.Fatal("invalid query accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid request was dispatched")
	}
	for _, provider := range []string{"deepseek", "searxng"} {
		if _, err := snapshot.Search(context.Background(), Request{Provider: provider, Query: "redirect"}); err != nil {
			t.Fatal(err)
		}
	}
	if leaked.Load() {
		t.Fatal("search credentials crossed origins")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snapshot.Search(ctx, Request{Provider: "deepseek", Query: "cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not preserved", err)
	}
	for _, code := range []int{401, 402, 403, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code); fmt.Fprint(w, "sensitive") }))
			defer server.Close()
			_, err := requestJSON(context.Background(), harness.SearchProviderConfig{Timeout: 1}, "GET", server.URL, nil, nil)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("remote failure or secret sanitization")
			}
		})
	}
}

func TestPinnedSearchSnapshotFailureClassification(t *testing.T) {
	isolatedEnvironment(t)
	m := mustManager(t, filepath.Join(t.TempDir(), "settings.json"), Defaults())
	pin, err := m.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	version := m.versionPath(pin.Ref.Providers["searxng"])
	if err := os.WriteFile(version, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(&pin.Ref); !errors.Is(err, harness.ErrPinnedSearchConfigInvalid) {
		t.Fatal("malformed version", err)
	}
	if err := os.WriteFile(version, make([]byte, maxSettingsBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(&pin.Ref); !errors.Is(err, harness.ErrPinnedSearchConfigInvalid) {
		t.Fatal("oversized version", err)
	}
	if _, err := m.Snapshot(nil); err != nil {
		t.Fatal("fresh run could not rebuild oversized version", err)
	}
	if _, err := m.Snapshot(&pin.Ref); err != nil {
		t.Fatal("rebuilt version failed digest validation", err)
	}
	if err := os.Remove(version); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(&pin.Ref); !errors.Is(err, harness.ErrPinnedSearchConfigInvalid) {
		t.Fatal("missing version", err)
	}
	if err := os.Mkdir(version, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(&pin.Ref); err == nil || errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatal("I/O error without version data must remain retryable", err)
	}
	if _, err := m.Snapshot(nil); err == nil || errors.Is(err, harness.ErrExecutionUnresumable) {
		t.Fatal("fresh run ignored a version I/O error", err)
	}
}

func TestSwitchPersistsWithoutRestartAndPinsExistingExecution(t *testing.T) {
	isolatedEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/res/") {
			fmt.Fprint(w, `{"web":{"results":[{"title":"Brave","url":"https://example.test/brave"}]}}`)
		} else {
			fmt.Fprint(w, `{"results":[{"title":"result","url":"https://example.test/result"}]}`)
		}
	}))
	defer server.Close()
	cfg := harness.SearchConfig{DefaultProvider: "brave", Providers: map[string]harness.SearchProviderConfig{"brave": {BaseURL: server.URL, APIKey: "brave-key"}, "deepseek": {BaseURL: server.URL, APIKey: "deepseek-key"}, "searxng": {BaseURL: server.URL}}}
	path := filepath.Join(t.TempDir(), "search-providers.json")
	daemon := mustManager(t, path, cfg)
	before, err := daemon.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A different manager represents the configuration CLI. The original daemon
	// remains alive and must observe all selections without reconstruction.
	configCLI := mustManager(t, path, cfg)
	for _, name := range []string{"deepseek", "searxng", "brave"} {
		if _, err := configCLI.Select(context.Background(), name); err != nil {
			t.Fatal(err)
		}
		next, err := daemon.Snapshot(nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := next.Search(context.Background(), Request{Query: "selected provider"})
		if err != nil || out.Provider != name {
			t.Fatal("live selection did not apply", name, err)
		}
		old, err := daemon.Snapshot(&before.Ref)
		if err != nil {
			t.Fatal(err)
		}
		out, err = old.Search(context.Background(), Request{Query: "resume"})
		if err != nil || out.Provider != "brave" {
			t.Fatal("existing execution changed provider", err)
		}
	}
	state, err := daemon.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range state.Providers {
		if !provider.Configured {
			t.Fatal("switch lost provider configuration")
		}
	}
	visible, _ := json.Marshal(state)
	if strings.Contains(string(visible), "brave-key") || strings.Contains(string(visible), "deepseek-key") {
		t.Fatal("status disclosed credentials")
	}
	rotated := "new-key"
	if _, err := configCLI.Update(context.Background(), Update{Provider: "brave", ExpectedRevision: state.Revision, APIKey: &rotated}); err != nil {
		t.Fatal(err)
	}
	old, err := daemon.Snapshot(&before.Ref)
	if err != nil || old.providers["brave"].APIKey != "brave-key" {
		t.Fatal("credential rotation broke existing snapshot", err)
	}
	current, err := daemon.Snapshot(nil)
	if err != nil || current.providers["brave"].APIKey != rotated {
		t.Fatal("credential update was not observed", err)
	}
}

func TestConfigurationCardResumesOriginalQueryAndCanCancel(t *testing.T) {
	isolatedEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "original query" {
			t.Error("original query was changed")
		}
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "search-providers.json")
	m := mustManager(t, path, Defaults())
	snapshot, _ := m.Snapshot(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := snapshot.SearchInteractive(ctx, Request{Provider: "searxng", Query: "original query"})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		state, _ := m.Status()
		if len(state.AwaitingConfiguration) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("configuration request was not exposed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cli := mustManager(t, path, Defaults())
	state, _ := cli.Status()
	address := server.URL
	if _, err := cli.Update(ctx, Update{Provider: "searxng", ExpectedRevision: state.Revision, BaseURL: &address}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("pending search did not resume", err)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := snapshot.SearchInteractive(cancelled, Request{Provider: "deepseek", Query: "cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatal("configuration wait ignored cancellation")
	}
}
