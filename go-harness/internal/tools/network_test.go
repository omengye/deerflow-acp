package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestWebFetchInheritsUnifiedProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "page.invalid" {
			t.Errorf("wrong target %s", r.URL.Host)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "fetched through shared proxy")
	}))
	defer proxy.Close()
	policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: proxy.URL}}
	service, err := newWebService(harness.BuiltinToolConfig{Name: "web_fetch"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer service.client.CloseIdleConnections()
	content, err := service.fetch(context.Background(), webFetchInput{URL: "http://page.invalid"})
	if err != nil || !strings.Contains(content, "fetched through shared proxy") {
		t.Fatal(content, err)
	}
}
