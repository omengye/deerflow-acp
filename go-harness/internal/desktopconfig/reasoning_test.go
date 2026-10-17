package desktopconfig

import (
	"slices"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSessionReasoningSettings(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	modelByName(t, doc, "first")["reasoning_efforts"] = []string{"low", "high", "xhigh", "max"}
	modelByName(t, doc, "first")["thinking_parameter"] = "enable_thinking"
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ModelReasoningEfforts[cfg.Model], []string{"low", "high", "xhigh", "max"}) {
		t.Fatalf("capabilities=%v", cfg.ModelReasoningEfforts)
	}
	if cfg.ModelThinking[cfg.Model].Parameter != "enable_thinking" {
		t.Fatal("thinking capability lost on save/load")
	}
	first := modelByName(t, saved, "first")
	for _, parameter := range []string{"not-a-contract", "claude"} {
		first["thinking_parameter"] = parameter
		if _, err := Run(options, "save", mustJSON(t, saved)); err == nil {
			t.Fatalf("accepted thinking contract %s", parameter)
		}
	}
	first["thinking_parameter"] = "enable_thinking"
	for _, tc := range []struct {
		provider string
		efforts  []string
	}{
		{"claude", []string{"high"}}, {"openai", []string{"high", "high"}}, {"openai", []string{"made-up"}},
	} {
		first["provider"], first["reasoning_efforts"] = tc.provider, tc.efforts
		if _, err := Run(options, "save", mustJSON(t, saved)); err == nil {
			t.Fatalf("accepted %s %v", tc.provider, tc.efforts)
		}
	}
	first["provider"], first["reasoning_efforts"] = "openai", nil
	saved = run(t, options, "save", saved)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModelReasoningEfforts[cfg.Model]) != 0 {
		t.Fatal("cleared capabilities survived save")
	}
	modelByName(t, saved, "first")["thinking_parameter"] = "unsupported"
	saved = run(t, options, "save", saved)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelThinking[cfg.Model].Availability() != "unsupported" || modelByName(t, saved, "first")["thinking_parameter"] != "unsupported" {
		t.Fatal("explicit unsupported capability lost on save/load")
	}
}
