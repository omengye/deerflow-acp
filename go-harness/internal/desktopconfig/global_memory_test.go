package desktopconfig

import (
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestGlobalMemorySettingsSaveLaunchRoundTrip(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	if readObject(doc, "memory")["include_global"] != true {
		t.Fatal("global read default absent")
	}
	readObject(doc, "runtime")["memory_scope"] = "global"
	saved := run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.MemoryScope != harness.MemoryGlobal || cfg.MemoryIncludeGlobal == nil || !*cfg.MemoryIncludeGlobal {
		t.Fatalf("saved global scope: %v", err)
	}
	readObject(saved, "runtime")["memory_scope"] = "session"
	readObject(saved, "memory")["include_global"] = false
	run(t, options, "save", saved)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.MemoryIncludeGlobal == nil || *cfg.MemoryIncludeGlobal || cfg.MemoryScope != harness.MemorySession {
		t.Fatalf("saved disabled sharing: %v", err)
	}
}
