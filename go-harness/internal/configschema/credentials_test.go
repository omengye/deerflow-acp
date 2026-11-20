package configschema

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const credentialFixture = `# DeerFlow configuration
models:
  - name: deepseek
    use: langchain_openai:ChatOpenAI
    model: deepseek-chat
    api_key: mock-model   # main key
    base_url: https://api.deepseek.com/v1
  - name: 中文模型
    api_key: "mock-double-\"quoted\"-0123"
  - {name: 模型二, model: m2, api_key: mock-flow}
  - name: single
    api_key: 'mock-single-''quoted''-0123'
  - name: env
    api_key: $OPENAI_API_KEY
  - name: ref
    api_key: ${secret:ALREADY_STORED}
  - name: block
    api_key: |
      mock-block
  - name: anchored
    api_key: &shared mock-anchor
  - name: alias
    api_key: *shared
  - name: placeholder
    api_key: not-configured
  - name: proxied
    base_url: https://user:pass@models.example/v1
tools:
  - {name: web_search, api_key: mock-search, max_results: 5}
  - name: web_fetch
    https_proxy: http://user:pass@proxy.example:8080
  - name: qiniu_upload
    access_key: mock-tool
network:
  proxy:
    enabled: true
    url: http://proxyuser:proxypass@proxy.example:3128
search:
  providers:
    brave:
      api_key: mock-brave
      https_proxy: http://u:p@proxy.example:1
mcp:
  host_recipes:
    - id: github
      workspace: /tmp/x
      servers:
        - name: gh
          command: /usr/bin/gh-mcp
          env:
            GITHUB_TOKEN: mock-mcp
            LOG_LEVEL: debug
        - name: remote
          transport: http
          url: https://mcp.example/mcp
          headers: {Authorization: "Bearer mock-header", X-Trace: "1"}
local_acp:
  max_output_tokens: 4096
channels:
  feishu:
    app_secret: mock-channel
`

type credentialWant struct{ field, secret, value, reason string }

var credentialWants = []credentialWant{
	{"models[deepseek].api_key", "MODEL_DEEPSEEK_API_KEY", "mock-model", ""},
	{"models[中文模型].api_key", "MODEL_API_KEY", `mock-double-"quoted"-0123`, ""},
	{"models[模型二].api_key", "MODEL_API_KEY", "mock-flow", ""},
	{"models[single].api_key", "MODEL_SINGLE_API_KEY", "mock-single-'quoted'-0123", ""},
	{"models[block].api_key", "MODEL_BLOCK_API_KEY", "mock-block\n", "block scalar"},
	{"models[anchored].api_key", "MODEL_ANCHORED_API_KEY", "mock-anchor", "YAML anchor"},
	{"models[proxied].base_url", "MODEL_PROXIED_BASE_URL", "https://user:pass@models.example/v1", ""},
	{"tools[web_search].api_key", "WEB_SEARCH_API_KEY", "mock-search", ""},
	{"tools[web_fetch].https_proxy", "WEB_FETCH_HTTPS_PROXY", "http://user:pass@proxy.example:8080", ""},
	{"search.providers.brave.api_key", "SEARCH_BRAVE_API_KEY", "mock-brave", ""},
	{"search.providers.brave.https_proxy", "SEARCH_BRAVE_HTTPS_PROXY", "http://u:p@proxy.example:1", "only api_key"},
	{"network.proxy.url", "NETWORK_PROXY_URL", "http://proxyuser:proxypass@proxy.example:3128", ""},
	{"mcp.host_recipes[github].servers[gh].env.GITHUB_TOKEN", "MCP_GH_GITHUB_TOKEN", "mock-mcp", ""},
	{"mcp.host_recipes[github].servers[remote].headers.Authorization", "MCP_REMOTE_AUTHORIZATION", "Bearer mock-header", ""},
	{"tools[qiniu_upload].access_key", "TOOLS_QINIU_UPLOAD_ACCESS_KEY", "mock-tool", "does not read"},
	{"channels.feishu.app_secret", "CHANNELS_FEISHU_APP_SECRET", "mock-channel", "does not read"},
}

func checkCredentials(t *testing.T, found []PlaintextCredential) {
	t.Helper()
	if len(found) != len(credentialWants) {
		for _, c := range found {
			t.Logf("found %s (%s)", c.Field, c.Reason)
		}
		t.Fatalf("found %d credentials; want %d", len(found), len(credentialWants))
	}
	for i, want := range credentialWants {
		got := found[i]
		if got.Field != want.field || got.Secret != want.secret || got.Value != want.value || (want.reason == "") != (got.Reason == "") || !strings.Contains(got.Reason, want.reason) {
			t.Errorf("credential %d = %s %s %q (%s); want %s %s %q (%s)", i, got.Field, got.Secret, got.Value, got.Reason, want.field, want.secret, want.value, want.reason)
		}
	}
}

func TestReplaceCredentialsKeepsEverythingElse(t *testing.T) {
	for _, variant := range []struct {
		name   string
		encode func(string) string
	}{
		{"lf", func(s string) string { return s }},
		{"crlf", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }},
		{"bom", func(s string) string { return "\xef\xbb\xbf" + s }},
	} {
		t.Run(variant.name, func(t *testing.T) {
			raw := []byte(variant.encode(credentialFixture))
			found, err := FindPlaintextCredentials(raw)
			if err != nil {
				t.Fatal(err)
			}
			checkCredentials(t, found)
			names := map[string]string{}
			for i, c := range found {
				if c.Reason == "" {
					names[c.Field] = c.Secret
					if c.Secret == "MODEL_API_KEY" {
						names[c.Field] = c.Secret + "_" + string(rune('2'+i))
					}
				}
			}
			requested := len(names)
			out, err := ReplaceCredentials(raw, names)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != requested {
				t.Fatal("ReplaceCredentials changed its argument")
			}
			want := variant.encode(credentialFixture)
			for _, r := range []struct{ old, new string }{
				{"api_key: mock-model   # main key", "api_key: ${secret:MODEL_DEEPSEEK_API_KEY}   # main key"},
				{`api_key: "mock-double-\"quoted\"-0123"`, `api_key: "${secret:MODEL_API_KEY_3}"`},
				{"{name: 模型二, model: m2, api_key: mock-flow}", `{name: 模型二, model: m2, api_key: "${secret:MODEL_API_KEY_4}"}`},
				{"api_key: 'mock-single-''quoted''-0123'", "api_key: '${secret:MODEL_SINGLE_API_KEY}'"},
				{"base_url: https://user:pass@models.example/v1", "base_url: ${secret:MODEL_PROXIED_BASE_URL}"},
				{"{name: web_search, api_key: mock-search, max_results: 5}", `{name: web_search, api_key: "${secret:WEB_SEARCH_API_KEY}", max_results: 5}`},
				{"https_proxy: http://user:pass@proxy.example:8080", "https_proxy: ${secret:WEB_FETCH_HTTPS_PROXY}"},
				{"api_key: mock-brave", "api_key: ${secret:SEARCH_BRAVE_API_KEY}"},
				{"url: http://proxyuser:proxypass@proxy.example:3128", "url: ${secret:NETWORK_PROXY_URL}"},
				{"GITHUB_TOKEN: mock-mcp", "GITHUB_TOKEN: ${secret:MCP_GH_GITHUB_TOKEN}"},
				{`{Authorization: "Bearer mock-header", X-Trace: "1"}`, `{Authorization: "${secret:MCP_REMOTE_AUTHORIZATION}", X-Trace: "1"}`},
			} {
				if strings.Count(want, r.old) != 1 {
					t.Fatalf("fixture does not hold %q once", r.old)
				}
				want = strings.Replace(want, r.old, r.new, 1)
			}
			if string(out) != want {
				t.Fatalf("rewritten config:\n%s\nwant:\n%s", out, want)
			}
			left, err := FindPlaintextCredentials(out)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range left {
				if c.Reason == "" {
					t.Errorf("%s is still plain text", c.Field)
				}
			}
			refs, err := SecretReferenceFields(out)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(refs["NETWORK_PROXY_URL"], []string{"network.proxy.url"}) || !reflect.DeepEqual(refs["ALREADY_STORED"], []string{"models[ref].api_key"}) || len(refs) != len(names)+1 {
				t.Fatalf("references = %v", refs)
			}
		})
	}
}

func TestReplaceCredentialsRefusesWhatItCannotRewrite(t *testing.T) {
	raw := []byte(credentialFixture)
	for field, want := range map[string]string{
		"models[block].api_key":              "block scalar",
		"models[anchored].api_key":           "YAML anchor",
		"search.providers.brave.https_proxy": "only api_key",
		"channels.feishu.app_secret":         "does not read",
		"models[env].api_key":                "no longer holds",
		"models[deepseek].model":             "no longer holds",
	} {
		if _, err := ReplaceCredentials(raw, map[string]string{field: "NAME"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("replace %s = %v; want %q", field, err, want)
		}
	}
	if _, err := ReplaceCredentials(raw, map[string]string{"models[deepseek].api_key": "lower"}); err == nil || !strings.Contains(err.Error(), "invalid secret name") {
		t.Errorf("invalid name = %v", err)
	}
	for _, bad := range []string{"", "- a\n- b\n", "a: 1\n---\nb: 2\n", "a: 1\na: 2\n", "a: [\n"} {
		if _, err := FindPlaintextCredentials([]byte(bad)); err == nil {
			t.Errorf("FindPlaintextCredentials(%q) accepted an invalid config", bad)
		}
	}
	// Two settings that share a field name cannot be selected apart.
	twice := "models:\n  - name: a\n    api_key: mock-one\n  - name: a\n    api_key: mock-two\n"
	found, err := FindPlaintextCredentials([]byte(twice))
	if err != nil || len(found) != 2 || !strings.Contains(found[0].Reason, "same name") || !strings.Contains(found[1].Reason, "same name") {
		t.Fatalf("duplicate names = %+v, %v", found, err)
	}
	// A value under an anchor would change every alias of it.
	shared := "base: &base\n  api_key: mock-base\nmodels:\n  - name: a\n    api_key: mock-solo\nsearch: {providers: {brave: *base}}\n"
	found, err = FindPlaintextCredentials([]byte(shared))
	if err != nil || len(found) != 2 || found[0].Field != "models[a].api_key" || found[0].Reason != "" || found[1].Field != "search.providers.brave.api_key" || !strings.Contains(found[1].Reason, "YAML anchor") {
		t.Fatalf("anchored values = %+v, %v", found, err)
	}
}

func TestSecretNameAndCredentialKey(t *testing.T) {
	for parts, want := range map[string]string{
		"MODEL|deepseek|API_KEY":     "MODEL_DEEPSEEK_API_KEY",
		"MODEL|gpt-4o mini|API_KEY":  "MODEL_GPT_4O_MINI_API_KEY",
		"web_search|API_KEY":         "WEB_SEARCH_API_KEY",
		"MCP|--x--|Authorization":    "MCP_X_AUTHORIZATION",
		"1password|TOKEN":            "SECRET_1PASSWORD_TOKEN",
		"模型":                         "SECRET",
		strings.Repeat("a_", 100):    strings.TrimRight(strings.Repeat("A_", 64), "_"),
		"channels.feishu.app_secret": "CHANNELS_FEISHU_APP_SECRET",
	} {
		if got := SecretName(strings.Split(parts, "|")...); got != want {
			t.Errorf("SecretName(%q) = %q; want %q", parts, got, want)
		}
	}
	for key, want := range map[string]bool{"api_key": true, "X-API-Key": true, "GITHUB_TOKEN": true, "Authorization": true, "aws_secret_access_key": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_ACCESS_KEY_ID": false, "client_secret": true, "password": true, "max_output_tokens": false, "auth_header": false, "token_budget": false, "model": false} {
		if credentialKey(key) != want {
			t.Errorf("credentialKey(%q) = %v", key, !want)
		}
	}
}

// The parser reports columns in characters, which locateScalar relies on.
func TestYAMLColumnsCountCharacters(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("{名字: 值, key: v}\n"), &doc); err != nil {
		t.Fatal(err)
	}
	value := doc.Content[0].Content[3]
	// 14 characters precede the value; counting bytes would give 20.
	if value.Value != "v" || value.Column != 14 {
		t.Fatalf("value %q at column %d", value.Value, value.Column)
	}
}
