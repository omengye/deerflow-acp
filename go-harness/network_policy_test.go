package deerflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestNetworkToggleRejectsChangedExtensionRestore(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "enable"
		if enabled {
			name = "disable"
		}
		t.Run(name, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.BuiltinTools = []harness.BuiltinToolConfig{{Name: "web_fetch"}}
			cfg.Network = harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: enabled, URL: "http://user:private-proxy-secret@127.0.0.1:8888"}}
			db, err := sqlite.Open(filepath.Join(cfg.DataDir, "network.db"))
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
			req := harness.RunRequest{Session: harness.Session{ID: "network", CWD: workspace, Mode: "plan"}}
			first, err := extensionFactory(cfg, manager, registry, nil)(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Cleanup()
			if strings.Contains(string(first.State), "private-proxy-secret") || strings.Contains(string(first.State), "127.0.0.1:8888") {
				t.Fatal("network credentials persisted in execution snapshot")
			}
			restored, err := extensionFactory(cfg, manager, registry, nil)(context.Background(), req, first.State)
			if err != nil {
				t.Fatal("unchanged network rejected", err)
			}
			defer restored.Cleanup()
			cfg.Network.Proxy.Enabled = !enabled
			if _, err := extensionFactory(cfg, manager, registry, nil)(context.Background(), req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatal("network toggle accepted while restoring old execution", err)
			}
		})
	}
}

func TestNetworkPolicyPinsBackgroundRestore(t *testing.T) {
	for _, version := range []int{1, 3} {
		cfg := Config{}
		baseline, err := backgroundHostPolicyVersion(cfg, version)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Network.Proxy.URL = "http://user:private-proxy-secret@proxy.invalid:8080"
		disabled, err := backgroundHostPolicyVersion(cfg, version)
		if err != nil || baseline != disabled {
			t.Fatal("disabled retained address changed legacy background policy", err)
		}
		cfg.Network.Proxy.Enabled = true
		enabled, err := backgroundHostPolicyVersion(cfg, version)
		if err != nil || enabled == disabled {
			t.Fatal("enabled proxy not pinned by background policy", err)
		}
		cfg.Network.Proxy.NoProxy = []string{"internal.test"}
		bypass, err := backgroundHostPolicyVersion(cfg, version)
		if err != nil || bypass == enabled {
			t.Fatal("bypass list not pinned by background policy", err)
		}
		cfg.Network.Proxy.Enabled = false
		disabledAgain, err := backgroundHostPolicyVersion(cfg, version)
		if err != nil || disabledAgain != baseline {
			t.Fatal("disabled network did not restore legacy background policy", err)
		}
	}
}
