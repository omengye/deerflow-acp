package desktopconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
	nativetools "github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func toolByName(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	for _, item := range list(document, "tools") {
		value := item.(map[string]any)
		if str(value, "name", "") == name {
			return value
		}
	}
	t.Fatalf("missing tool %s", name)
	return nil
}

func loadToolSettings(t *testing.T, options Options) (deerflow.Config, map[string]harness.BuiltinToolConfig) {
	t.Helper()
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	byName := map[string]harness.BuiltinToolConfig{}
	for _, setting := range cfg.BuiltinTools {
		byName[setting.Name] = setting
	}
	return cfg, byName
}

func TestToolSettingsSaveLaunchAndNativeProxyRequest(t *testing.T) {
	requests := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Proxy-Authorization")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("界", 60))
	}))
	defer proxy.Close()
	address, _ := url.Parse(proxy.URL)
	address.User = url.UserPassword("fixture", "proxy-fixture-secret")
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	items := []any{}
	for _, name := range configschema.ToolNames() {
		items = append(items, map[string]any{"name": name})
	}
	raw["tools"] = items
	search := toolByName(t, raw, "web_search")
	search["api_key"], search["max_results"], search["timeout"] = "fixture-search-secret", 7, 12
	search["use"] = "deerflow.community.brave_search.tools:web_search_tool"
	search["group"] = "unregistered-legacy-group"
	search["old_options"] = map[string]any{"credential": "fixture-old-secret"}
	fetch := toolByName(t, raw, "web_fetch")
	fetch["https_proxy"], fetch["max_output_chars"], fetch["timeout"] = address.String(), 17, 2
	toolByName(t, raw, "image_search")["max_results"] = 31
	toolByName(t, raw, "glob")["max_results"] = 301
	toolByName(t, raw, "grep")["max_results"] = 211
	cli := toolByName(t, raw, "host_opencli")
	cli["executable"], cli["timeout"], cli["allowed_sites"] = "./fixture-opencli.cmd", 71, []any{}
	readObject(raw, "sandbox")["allow_host_tools"] = true
	readObject(raw, "sandbox")["old_option"] = map[string]any{"token": "fixture-sandbox-secret"}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	encoded, _ := json.Marshal(doc)
	for _, secret := range []string{"fixture-search-secret", "proxy-fixture-secret", "fixture-old-secret", "fixture-sandbox-secret", "deerflow.community"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("snapshot leaked %s", secret)
		}
	}
	if _, exists := doc["tool_groups"]; exists {
		t.Fatal("obsolete tool groups still editable")
	}
	if len(doc["tool_diagnostics"].([]string)) < 3 {
		t.Fatal("retained parameters have no diagnostics")
	}
	slices.Reverse(list(doc, "tools"))
	run(t, options, "save", doc)
	// A second unrelated save must retain credentials by name after reordering.
	run(t, options, "save", run(t, options, "snapshot", nil))
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(toolByName(t, saved, "web_search")["old_options"], search["old_options"]) || !reflect.DeepEqual(saved["tool_groups"], raw["tool_groups"]) || !reflect.DeepEqual(readObject(saved, "sandbox")["old_option"], readObject(raw, "sandbox")["old_option"]) {
		t.Fatal("unrelated save lost migration data")
	}
	for _, item := range list(saved, "tools") {
		if _, exists := item.(map[string]any)["use"]; exists {
			t.Fatal("legacy class path reintroduced")
		}
	}
	cfg, settings := loadToolSettings(t, options)
	if !cfg.HostToolsAllowed || len(settings) != 13 || settings["web_search"].APIKey != "fixture-search-secret" || settings["web_search"].MaxResults != 7 || settings["web_search"].Timeout != 12 || settings["web_fetch"].HTTPSProxy != address.String() || settings["web_fetch"].Timeout != 2 {
		t.Fatal("tool routing or credentials not consumed")
	}
	if settings["image_search"].MaxResults != 31 || settings["glob"].MaxResults != 301 || settings["grep"].MaxResults != 211 || settings["host_opencli"].Timeout != 71 || settings["host_opencli"].Executable != filepath.Join(filepath.Dir(options.Config), "fixture-opencli.cmd") {
		t.Fatal("native limits or OpenCLI path not consumed")
	}
	if settings["host_opencli"].AllowedSites == nil || len(opencli.AllowedSites(settings["host_opencli"])) != 0 {
		t.Fatal("empty site restriction widened")
	}
	web, cleanup, err := nativetools.WebTools(settings["web_fetch"])
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	result, err := web.(tool.InvokableTool).InvokableRun(context.Background(), `{"url":"http://fixture.invalid/page"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, strings.Repeat("界", 17)+"\n\n[truncated]") || strings.Contains(result, strings.Repeat("界", 18)) {
		t.Fatal("saved output bound not honored by web_fetch")
	}
	select {
	case auth := <-requests:
		if auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture:proxy-fixture-secret")) {
			t.Fatal("saved proxy credentials not used")
		}
	default:
		t.Fatal("configured proxy not used")
	}
	doc = run(t, options, "snapshot", nil)
	toolByName(t, doc, "host_opencli")["allowed_sites"] = nil
	toolByName(t, doc, "web_search")["clear_api_key"] = true
	toolByName(t, doc, "web_fetch")["clear_https_proxy"] = true
	run(t, options, "save", doc)
	_, settings = loadToolSettings(t, options)
	if settings["web_search"].APIKey != "" || settings["web_fetch"].HTTPSProxy != "" || settings["host_opencli"].AllowedSites != nil || len(opencli.AllowedSites(settings["host_opencli"])) == 0 {
		t.Fatal("explicit clear or null site defaults lost")
	}
}

func TestToolSettingsRejectInvalidDraftAndNativeParameters(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"web_search", "max_results", 21}, {"web_search", "timeout", 1.5},
		{"web_fetch", "max_output_chars", 50001}, {"web_fetch", "https_proxy", "file:///invalid"},
		{"image_search", "max_results", -1}, {"glob", "max_results", 1001}, {"grep", "max_results", 501},
		{"host_opencli", "allowed_sites", []any{"web", "web"}}, {"host_opencli", "allowed_sites", []any{"--unsafe"}},
		{"host_opencli", "executable", "bad\npath"}, {"host_opencli", "timeout", "5"},
	} {
		t.Run(tc.name+"/"+tc.key, func(t *testing.T) {
			options := setup(t)
			doc := run(t, options, "snapshot", nil)
			entry := configschema.ToolDefaults(tc.name)
			entry[tc.key] = tc.value
			doc["tools"] = []any{entry}
			before, _ := os.ReadFile(options.Config)
			request, _ := json.Marshal(doc)
			if _, err := Run(options, "save", request); err == nil {
				t.Fatal("invalid draft accepted")
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid save changed source")
			}
			raw, _ := readYAML(options.Config)
			raw["tools"] = []any{map[string]any{"name": tc.name, tc.key: tc.value}}
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err == nil {
				t.Fatal("invalid native config accepted")
			}
		})
	}
}

func TestToolSettingsRejectOpaqueEditsAndCredentialRebinding(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	doc["tools"] = []any{map[string]any{"name": "web_search", "api_key": "retained-fixture-secret"}}
	run(t, options, "save", doc)
	for _, change := range []func(map[string]any){
		func(d map[string]any) { toolByName(t, d, "web_search")["use"] = "builtin:web_search" },
		func(d map[string]any) { toolByName(t, d, "web_search")["executable"] = "ignored" },
		func(d map[string]any) { toolByName(t, d, "web_search")["advanced"] = map[string]any{} },
		func(d map[string]any) { toolByName(t, d, "web_search")["original_name"] = "image_search" },
		func(d map[string]any) { d["tools"] = nil },
		func(d map[string]any) { d["tool_groups"] = []any{} },
		func(d map[string]any) { readObject(d, "sandbox")["advanced"] = map[string]any{} },
		func(d map[string]any) { readObject(d, "sandbox")["allow_host_tools"] = "true" },
	} {
		doc = run(t, options, "snapshot", nil)
		change(doc)
		before, _ := os.ReadFile(options.Config)
		request, _ := json.Marshal(doc)
		if _, err := Run(options, "save", request); err == nil {
			t.Fatal("opaque edit or identity change accepted")
		}
		after, _ := os.ReadFile(options.Config)
		if !bytes.Equal(before, after) {
			t.Fatal("rejected save changed source")
		}
	}
	// Replacing the row without its original identity is explicitly fresh.
	doc = run(t, options, "snapshot", nil)
	doc["tools"] = []any{configschema.ToolDefaults("web_search")}
	run(t, options, "save", doc)
	_, settings := loadToolSettings(t, options)
	if settings["web_search"].APIKey != "" {
		t.Fatal("fresh tool borrowed an old credential")
	}
}

func TestToolSettingsRetainedInvalidParameterCannotSaveAsValid(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	// timeout is not consumed by read_file, but the native decoder still has
	// to parse a retained historical value. Do not accept an unlaunchable save.
	toolByName(t, raw, "read_file")["timeout"] = "invalid"
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if _, exposed := toolByName(t, doc, "read_file")["timeout"]; exposed {
		t.Fatal("unused parameter exposed as editable")
	}
	before, _ := os.ReadFile(options.Config)
	request, _ := json.Marshal(doc)
	if _, err := Run(options, "save", request); err == nil {
		t.Fatal("unlaunchable merged configuration accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed retained configuration")
	}
}
