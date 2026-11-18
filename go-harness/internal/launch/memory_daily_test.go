package launch

import (
	"fmt"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestMemoryStage2DailyAttemptsConfig(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 200, 1000000, 1000001} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var cfg deerflow.Config
			connections := 0
			_, err := ApplyConfig(writeRoleConfig(t, roleModels+fmt.Sprintf("memory:\n  daily_attempts: %d\n", limit)), &cfg, &connections, nil)
			if limit < 0 || limit > 1000000 {
				if err == nil {
					t.Fatal("invalid cap accepted")
				}
				return
			}
			if err != nil || cfg.MemoryDailyAttempts == nil || *cfg.MemoryDailyAttempts != limit {
				t.Fatalf("cap not applied: %v %v", cfg.MemoryDailyAttempts, err)
			}
		})
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := ApplyConfig(writeRoleConfig(t, roleModels), &cfg, &connections, nil); err != nil || cfg.MemoryDailyAttempts != nil {
		t.Fatalf("default overridden: %v %v", cfg.MemoryDailyAttempts, err)
	}
}
