package evolution

import (
	"strings"
	"testing"
)

const discoveryConfigBase = "enabled: true\nmoderation_model_name: primary\nevaluation_model_name: primary\ngeneration_model_name: primary\ngeneration:\n  enabled: true\nbudget:\n  max_daily_model_calls: 30\n  max_daily_tokens: 100000\n  max_job_seconds: 300\n  workers: 1\n"

func TestDiscoveryConfigurationRemovedAndDefaultsAutomatic(t *testing.T) {
	settings := AutomaticSettings("primary")
	if settings.DiscoveryOptions() == nil {
		t.Fatal("zero-config pipeline disabled")
	}
	for _, old := range []string{discoveryConfigBase, "enabled: false\n"} {
		if _, err := DecodeReviewConfig([]byte(old), map[string]bool{"primary": true}, "primary"); err == nil {
			t.Fatal("retired configuration accepted")
		}
	}
}

func TestDiscoveryConfigRejectsConflictsAndInvalidLimits(t *testing.T) {
	models := map[string]bool{"primary": true}
	active := discoveryConfigBase + "discovery:\n  operator_corrections_enabled: true\n"
	for _, bad := range []string{
		strings.Replace(active, "enabled: true", "enabled: false", 1),
		strings.Replace(active, "generation:\n  enabled: true", "generation:\n  enabled: false", 1),
		strings.Replace(active, "generation_model_name: primary", "generation_model_name: missing", 1),
		strings.Replace(active, "max_daily_model_calls: 30", "max_daily_model_calls: 0", 1),
		strings.Replace(active, "operator_corrections_enabled: true", "operator_corrections_enabled: yes", 1),
		active + "  repeat_threshold: 1\n", active + "  repeat_threshold: 17\n", active + "  repeat_threshold: 2.5\n",
		active + "  repeat_window_days: 0\n", active + "  repeat_window_days: 366\n",
		active + "  cooldown_hours: 0\n", active + "  cooldown_hours: 8761\n",
		active + "  max_daily_proposals: 0\n", active + "  max_daily_proposals: 101\n",
		active + "  max_pending_proposals: 0\n", active + "  max_pending_proposals: 1001\n",
		active + "  enabled: true\n", active + "  min_tool_calls: 5\n", active + "  repeat_threshold: null\n",
	} {
		if _, err := DecodeReviewConfig([]byte(bad), models, "primary"); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Errorf("invalid discovery configuration accepted: %q: %v", bad, err)
		}
	}
}
