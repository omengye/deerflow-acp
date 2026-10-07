package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredAutomaticCompactionAndOptInTokenBudget(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		flags          []string
		budget         int64
		compaction     bool
		wantError      bool
	}{
		{name: "defaults", compaction: true},
		{name: "enabled-with-default-thresholds", settings: "summarization:\n  enabled: true\n", compaction: true},
		{name: "explicit-opt-out", settings: "summarization:\n  enabled: false\n"},
		{name: "cli-opt-out", flags: []string{"--context-compaction=false"}},
		{name: "yaml-budget", settings: "local_acp:\n  max_run_tokens: 500000\n", budget: 500000, compaction: true},
		{name: "yaml-unlimited", settings: "local_acp:\n  max_run_tokens: 0\n", compaction: true},
		{name: "cli-budget-wins", settings: "local_acp:\n  max_run_tokens: 500000\n", flags: []string{"--max-tokens=750000"}, budget: 750000, compaction: true},
		{name: "cli-disables-yaml-budget", settings: "local_acp:\n  max_run_tokens: 500000\n", flags: []string{"--max-tokens=0"}, compaction: true},
		{name: "negative-budget", settings: "local_acp:\n  max_run_tokens: -1\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseRuntimeFlags(t, tc.flags...)
			if err != nil {
				t.Fatal(err)
			}
			explicit := map[string]bool{}
			for _, flag := range tc.flags {
				name, _, _ := strings.Cut(strings.TrimPrefix(flag, "--"), "=")
				explicit[name] = true
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.settings+"models:\n  - name: one\n    provider: openai\n    model: one\n"), 0600); err != nil {
				t.Fatal(err)
			}
			connections := 0
			_, err = ApplyConfig(path, &cfg, &connections, explicit)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "max_run_tokens") {
					t.Fatalf("invalid budget accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.Budget.MaxTokens != tc.budget || cfg.Compaction.Enabled != tc.compaction {
				t.Fatalf("budget=%+v compaction=%+v err=%v", cfg.Budget, cfg.Compaction, err)
			}
			if tc.compaction && (cfg.Compaction.ContextTokens != 0 || cfg.Compaction.ContextMessages != 0 || cfg.Compaction.KeepRecentMessages != 8) {
				t.Fatalf("inconsistent automatic defaults: %+v", cfg.Compaction)
			}
		})
	}
}
