package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestExecutionTimeDefaultsUnlimitedAndExplicitMultiDayLimits(t *testing.T) {
	flags, err := parseRuntimeFlags(t)
	if err != nil || flags.Budget.Timeout != 0 || flags.QueueTimeout != 0 || !flags.Compaction.Enabled || harness.DefaultBudgetLimits().Timeout != 0 {
		t.Fatal("default task time is bounded or compaction disabled", flags.Budget, err)
	}
	for _, seconds := range []int{-1, 0, 3 * 24 * 60 * 60} {
		local := ""
		if seconds >= 0 {
			local = fmt.Sprintf("local_acp:\n  run_timeout_seconds: %d\n  queue_timeout_seconds: %d\nsubagents:\n  timeout_seconds: %d\n", seconds, seconds, seconds)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(local+"models:\n  - name: fixture\n    model: fixture\n    provider: openai\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var cfg deerflow.Config
		connections := 0
		if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
			t.Fatal(seconds, err)
		}
		want := time.Duration(max(0, seconds)) * time.Second
		if cfg.Budget.Timeout != want || cfg.QueueTimeout != want {
			t.Fatal("configured timeout lost", cfg.Budget, cfg.QueueTimeout, want)
		}
	}
}
