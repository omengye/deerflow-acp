package desktopconfig

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func networkProxy(document map[string]any) map[string]any {
	return readObject(readObject(document, "network"), "proxy")
}

func TestNetworkSettingsRoundTripLaunchAndClear(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	proxy := networkProxy(doc)
	if proxy["enabled"] != false || !reflect.DeepEqual(proxy["no_proxy"], []string{"localhost", "127.0.0.1", "::1"}) {
		t.Fatal("unexpected network defaults")
	}
	const address = "http://fixture:network-private-password@127.0.0.1:7890"
	proxy["enabled"], proxy["url"] = true, address
	doc = run(t, options, "save", doc)
	if networkProxy(doc)["url"] != address || networkProxy(doc)["url_configured"] != true {
		t.Fatal("saved network address was not returned for editing")
	}
	doc = run(t, options, "snapshot", nil)
	if networkProxy(doc)["url"] != address {
		t.Fatal("reopening settings did not retain the visible network address")
	}
	var cfg deerflow.Config
	connections := 0
	source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Network.Proxy.Enabled || cfg.Network.Proxy.URL != address || len(cfg.ModelRoutes) == 0 {
		t.Fatal("launch did not apply proxy configuration")
	}
	for _, route := range cfg.ModelRoutes {
		if !reflect.DeepEqual(route.Network, cfg.Network) {
			t.Fatal("launch did not apply proxy to all model routes")
		}
	}
	encoded, _ := json.Marshal(source.Settings)
	if bytes.Contains(encoded, []byte("network-private-password")) || source.Settings.Fields["/network/proxy/enabled"].Value != true {
		t.Fatal("applied network settings leaked or lost state")
	}
	// A blank address still retains the saved value while toggling the switch.
	networkProxy(doc)["enabled"] = false
	networkProxy(doc)["url"] = ""
	doc = run(t, options, "save", doc)
	raw, _ := readYAML(options.Config)
	if networkProxy(raw)["url"] != address || networkProxy(raw)["enabled"] != false {
		t.Fatal("disabling proxy discarded its address")
	}
	// Legacy clients do not erase a section they do not understand.
	delete(doc, "network")
	doc = run(t, options, "save", doc)
	if networkProxy(doc)["url_configured"] != true || networkProxy(doc)["url"] != address {
		t.Fatal("legacy save erased network settings")
	}
	networkProxy(doc)["clear_url"] = true
	doc = run(t, options, "save", doc)
	if networkProxy(doc)["url_configured"] != false || networkProxy(doc)["url"] != "" {
		t.Fatal("explicit clear retained proxy address")
	}
	// Settings display references as written, without exposing environment values.
	const reference = "$DEERFLOW_TEST_VISIBLE_PROXY_URL"
	t.Setenv("DEERFLOW_TEST_VISIBLE_PROXY_URL", address)
	networkProxy(doc)["url"] = reference
	doc = run(t, options, "save", doc)
	if networkProxy(doc)["url"] != reference {
		t.Fatal("saved network environment reference was expanded")
	}
	doc = run(t, options, "snapshot", nil)
	if networkProxy(doc)["url"] != reference {
		t.Fatal("network snapshot expanded an environment reference")
	}
}

func TestModelProbeUsesNetworkDraftWithoutSaving(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Host != "model.example.test" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture:network-private-password")) {
			t.Error("model probe did not use the draft proxy credentials")
		}
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}})
	}))
	defer proxy.Close()
	address, _ := url.Parse(proxy.URL)
	address.User = url.UserPassword("fixture", "network-private-password")
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	model := modelByName(t, doc, "first")
	model["base_url"], model["api_key"] = "http://model.example.test/v1", "model-fixture-key"
	networkProxy(doc)["enabled"], networkProxy(doc)["url"] = true, address.String()
	before, _ := os.ReadFile(options.Config)
	result := run(t, options, "test-model", map[string]any{"model": model, "network": doc["network"]})
	if result["ok"] != true || requests.Load() != 1 {
		t.Fatalf("draft model probe failed: %v", result)
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("model connection test saved the draft")
	}
}

func TestNetworkProbeUsesDraftAndSanitizesFailure(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodConnect || r.Host != "example.com:443" {
			t.Error("proxy test did not verify the HTTPS tunnel")
		}
		http.Error(w, "proxy-private-error", http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	networkProxy(doc)["enabled"], networkProxy(doc)["url"] = true, proxy.URL
	networkProxy(doc)["no_proxy"] = []string{"*"} // Explicit test still checks the proxy.
	before, _ := os.ReadFile(options.Config)
	result := run(t, options, "test-network", map[string]any{"network": doc["network"]})
	encoded, _ := json.Marshal(result)
	if result["ok"] != false || requests.Load() != 1 || strings.Contains(string(encoded), "proxy-private-error") {
		t.Fatalf("unexpected network probe: %s", encoded)
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("proxy connection test saved the draft")
	}
}

func TestNetworkSettingsRejectInvalidDraftBeforeSave(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(p map[string]any) { p["enabled"] = true },
		func(p map[string]any) { p["url"] = "file:///private" },
		func(p map[string]any) { p["no_proxy"] = []string{"host,other"} },
		func(p map[string]any) { p["no_proxy"] = []string{"host name"} },
		func(p map[string]any) { p["url"] = "$invalid variable" },
		func(p map[string]any) { p["url"] = "http://127.0.0.1:7890"; p["enabled"] = true; p["clear_url"] = true },
	} {
		options := setup(t)
		doc := run(t, options, "snapshot", nil)
		before, _ := os.ReadFile(options.Config)
		change(networkProxy(doc))
		if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
			t.Fatal("invalid proxy draft was saved")
		}
		after, _ := os.ReadFile(options.Config)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid proxy draft changed the configuration")
		}
	}
}
