package evolution

import (
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const discoveryConfigBase = "enabled: true\nmoderation_model_name: primary\nevaluation_model_name: primary\ngeneration_model_name: primary\ngeneration:\n  enabled: true\nbudget:\n  max_daily_model_calls: 30\n  max_daily_tokens: 100000\n  max_job_seconds: 300\n  workers: 1\n"

func TestDiscoveryConfigExplicitOptInAndDefaults(t *testing.T) {
	models := map[string]bool{"primary": true}
	for _, suffix := range []string{"", "discovery:\n  operator_corrections_enabled: false\n", "discovery:\n  repeat_threshold: 3\n"} {
		settings, err := DecodeReviewConfig([]byte(discoveryConfigBase+suffix), models)
		if err != nil || settings.DiscoveryOptions() != nil {
			t.Fatalf("configuration without native opt-in activated discovery: %+v %v", settings, err)
		}
	}
	settings, err := DecodeReviewConfig([]byte(discoveryConfigBase+"discovery:\n  operator_corrections_enabled: true\n"), models)
	want := &harness.SkillDiscoveryOptions{RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
	if err != nil || !reflect.DeepEqual(settings.DiscoveryOptions(), want) {
		t.Fatalf("native defaults: %+v %v", settings.DiscoveryOptions(), err)
	}
	settings, err = DecodeReviewConfig([]byte("enabled: false\ndiscovery:\n  enabled: true\n  min_tool_calls: 5\n"), models)
	if err != nil || settings.DiscoveryOptions() != nil {
		t.Fatalf("legacy flag activated discovery: %+v %v", settings, err)
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
		if _, err := DecodeReviewConfig([]byte(bad), models); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
			t.Errorf("invalid discovery configuration accepted: %q: %v", bad, err)
		}
	}
}
