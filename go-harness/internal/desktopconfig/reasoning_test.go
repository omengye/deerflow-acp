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
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ModelReasoningEfforts[cfg.Model], []string{"low", "high", "xhigh", "max"}) {
		t.Fatalf("capabilities=%v", cfg.ModelReasoningEfforts)
	}
	first := modelByName(t, saved, "first")
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
	run(t, options, "save", saved)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModelReasoningEfforts[cfg.Model]) != 0 {
		t.Fatal("cleared capabilities survived save")
	}
}
