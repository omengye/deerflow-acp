package desktopconfig

import (
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestUnlimitedExecutionSettingsSaveAndLaunch(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	for _, seconds := range []float64{0, 3 * 24 * 60 * 60} {
		runtime := readObject(doc, "runtime")
		runtime["run_timeout_seconds"], runtime["queue_timeout_seconds"] = seconds, seconds
		readObject(doc, "subagents")["timeout_seconds"] = seconds
		doc = run(t, options, "save", doc)
		var cfg deerflow.Config
		connections := 0
		if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
			t.Fatal(err)
		}
		if cfg.Budget.Timeout != time.Duration(seconds)*time.Second || cfg.QueueTimeout != time.Duration(seconds)*time.Second || *cfg.Subagents.TimeoutSeconds != seconds {
			t.Fatal("settings time limit not applied", cfg.Budget, cfg.QueueTimeout, cfg.Subagents)
		}
	}
}
