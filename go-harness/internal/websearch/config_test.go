package websearch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyAndOfficialCredentialReuse(t *testing.T) {
	isolatedEnvironment(t)
	for _, test := range []struct {
		name, yaml              string
		wantBrave, wantDeepseek bool
	}{
		{"legacy", `tools: [{name: web_search, api_key: legacy-key, https_proxy: http://127.0.0.1:11808}]`, true, false},
		{"official", `models: [{base_url: https://api.deepseek.com/v1, api_key: official-key}]`, false, true},
		{"relay", `models: [{base_url: https://relay.example.test/v1, api_key: relay-key}]`, false, false},
		{"lookalike", `models: [{base_url: https://api.deepseek.com.attacker.test/v1, api_key: relay-key}]`, false, false},
		{"url-userinfo", `models: [{base_url: 'https://user:pass@api.deepseek.com/v1', api_key: key}]`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := SeedYAML([]byte(test.yaml))
			if err != nil {
				t.Fatal(err)
			}
			m := mustManager(t, "", cfg)
			state, _ := m.Status()
			for _, p := range state.Providers {
				if p.Name == "brave" && p.Configured != test.wantBrave || p.Name == "deepseek" && p.Configured != test.wantDeepseek {
					t.Fatal("incorrect credential reuse", p.Name)
				}
			}
		})
	}
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	t.Setenv("SEARXNG_BASE_URL", "https://search.example.test")
	t.Setenv("SEARXNG_TOKEN", "compat-token")
	cfg, err := SeedYAML([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := mustManager(t, "", cfg).Snapshot(nil)
	if err != nil || snapshot.providers["deepseek"].APIKey != "env-key" || snapshot.providers["searxng"].APIKey != "compat-token" {
		t.Fatal("environment defaults not consumed", err)
	}
	if snapshot.Ref.DefaultProvider != "deepseek" {
		t.Fatal("available provider was not selected automatically")
	}
}

func TestUserSkillCredentialFileReuse(t *testing.T) {
	isolatedEnvironment(t)
	profile, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// This test's profile is isolated by USERPROFILE on Windows.
	if os.Getenv("USERPROFILE") != profile {
		t.Skip("requires isolated Windows profile")
	}
	path := filepath.Join(profile, ".config", "deepseek-search", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"api_key":"local-skill-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := SeedYAML([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := mustManager(t, "", cfg).Snapshot(nil)
	if snapshot.providers["deepseek"].APIKey != "local-skill-key" {
		t.Fatal("documented skill credential was not reused")
	}
}

func TestSettingsValidationRedactionAndCompareAndSwap(t *testing.T) {
	isolatedEnvironment(t)
	for _, body := range []string{"search: {default_provider: invalid}", "search: {providers: {invalid: {}}}", "search: {providers: {searxng: {unknown: key}}}", "search: {providers: {searxng: {base_url: 'https://user:pass@search.test'}}}"} {
		cfg, err := SeedYAML([]byte(body))
		if err == nil {
			err = Validate(cfg)
		}
		if err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	m := mustManager(t, filepath.Join(t.TempDir(), "providers.json"), Defaults())
	state, _ := m.Status()
	address, key := "https://search.example.test", "fixture-secret-key"
	updated, err := m.Update(context.Background(), Update{Provider: "searxng", ExpectedRevision: state.Revision, BaseURL: &address, APIKey: &key, MakeDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background(), Update{Provider: "searxng", ExpectedRevision: state.Revision, ClearAPIKey: true}); err == nil {
		t.Fatal("stale update overwrote current configuration")
	}
	empty := ""
	updated, err = m.Update(context.Background(), Update{Provider: "searxng", ExpectedRevision: updated.Revision, APIKey: &empty})
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range updated.Providers {
		if provider.Name == "searxng" && !provider.APIKeyConfigured {
			t.Fatal("empty masked field removed Key")
		}
	}
	updated, err = m.Update(context.Background(), Update{Provider: "searxng", ExpectedRevision: updated.Revision, ClearAPIKey: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range updated.Providers {
		if p.Name == "searxng" && (!p.Configured || p.APIKeyConfigured) {
			t.Fatal("SearXNG key was required")
		}
	}
	visible, _ := json.Marshal(updated)
	if strings.Contains(string(visible), key) {
		t.Fatal("credential disclosed in status")
	}
	if _, err := ConfigureJSON(m, "search-configure", []byte(`{"provider":"searxng","unknown":"secret"}`)); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("unknown configuration fields accepted or echoed")
	}
	if _, err := ConfigureJSON(m, "search-select", []byte(`{"provider":"searxng"} {}`)); err == nil {
		t.Fatal("multiple JSON documents accepted")
	}
}
