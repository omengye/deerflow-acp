package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSharedProxyCoversSearchProvidersAndDeepSeekFetch(t *testing.T) {
	isolatedEnvironment(t)
	var global, explicit atomic.Int32
	handler := func(counter *atomic.Int32) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/res/v1/web/search":
				fmt.Fprint(w, `{"web":{"results":[]}}`)
			case "/v1/tools/fetch":
				fmt.Fprint(w, `{"content":"fetched through proxy"}`)
			default:
				fmt.Fprint(w, `{"results":[]}`)
			}
		})
	}
	proxy := httptest.NewServer(handler(&global))
	defer proxy.Close()
	override := httptest.NewServer(handler(&explicit))
	defer override.Close()
	config := harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{
		"brave":    {APIKey: "fixture", BaseURL: "http://brave.invalid"},
		"deepseek": {APIKey: "fixture", BaseURL: "http://deepseek.invalid"},
		"searxng":  {BaseURL: "http://searxng.invalid", HTTPSProxy: override.URL},
	}}
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: proxy.URL, NoProxy: []string{"internal.test"}}}
	manager, err := New("", config, policy)
	if err != nil {
		t.Fatal(err)
	}
	// A running manager owns an immutable copy, even if its caller edits a draft.
	policy.Proxy.NoProxy[0] = "*"
	snapshot, err := manager.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		if _, err := snapshot.Search(context.Background(), Request{Provider: name, Query: "test"}); err != nil {
			t.Fatal(name, err)
		}
	}
	content, err := snapshot.Fetch(context.Background(), "https://page.invalid")
	if err != nil || content != "fetched through proxy" {
		t.Fatal(content, err)
	}
	if global.Load() != 3 || explicit.Load() != 1 {
		t.Fatalf("global=%d explicit=%d", global.Load(), explicit.Load())
	}
	state, err := manager.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range state.Providers {
		if provider.HTTPSProxyConfigured != (provider.Name == "searxng") {
			t.Fatal("wrong independent proxy status")
		}
	}
}
