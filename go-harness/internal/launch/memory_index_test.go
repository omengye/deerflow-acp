package launch

import (
	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"testing"
	"time"
)

func TestMemoryStage4IndexRetryConfig(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"defaults", "", true},
		{"custom", "memory:\n  index_max_attempts: 5\n  index_retry_seconds: 2\n  index_max_retry_seconds: 10\n", true},
		{"negative", "memory:\n  index_max_attempts: -1\n", false},
		{"too_many", "memory:\n  index_max_attempts: 21\n", false},
		{"reversed", "memory:\n  index_retry_seconds: 30\n  index_max_retry_seconds: 10\n", false},
		{"overflow", "memory:\n  index_retry_seconds: 9223372036854775807\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg deerflow.Config
			connections := 0
			_, err := ApplyConfig(writeRoleConfig(t, roleModels+tc.body), &cfg, &connections, nil)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "defaults" && (cfg.MemoryIndex.MaxAttempts != 3 || cfg.MemoryIndex.RetryDelay != time.Minute || cfg.MemoryIndex.MaxRetryDelay != time.Hour) {
				t.Fatalf("%+v", cfg.MemoryIndex)
			}
			if tc.name == "custom" && (cfg.MemoryIndex.MaxAttempts != 5 || cfg.MemoryIndex.RetryDelay != 2*time.Second || cfg.MemoryIndex.MaxRetryDelay != 10*time.Second) {
				t.Fatalf("%+v", cfg.MemoryIndex)
			}
		})
	}
}
