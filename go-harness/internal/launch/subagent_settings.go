package launch

import (
	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
)

// AppliedSubagentSettings is a bounded, secret-free projection of host settings
// for new runs. Existing sessions and pinned task recipes can be more restrictive.
type AppliedSubagentSettings struct {
	Version              string `json:"version"`
	ConfiguredEnabled    bool   `json:"configured_enabled"`
	Enabled              bool   `json:"enabled"`
	EnabledSource        string `json:"enabled_source"`
	ToolPolicyAllowsTask bool   `json:"tool_policy_allows_task"`
	MaxConcurrent        int    `json:"max_concurrent"`
	ConcurrencySource    string `json:"concurrency_source"`
}

func appliedSubagentSettings(document map[string]any, controls configschema.SubagentControls, cfg *deerflow.Config, explicit map[string]bool) AppliedSubagentSettings {
	result := AppliedSubagentSettings{Version: "deerflow-subagent-settings-v1", ConfiguredEnabled: controls.Enabled,
		Enabled: !cfg.DisableSubagents && cfg.ToolPolicy.Allows("task"), ToolPolicyAllowsTask: cfg.ToolPolicy.Allows("task"),
		MaxConcurrent: controls.MaxConcurrent, EnabledSource: "default", ConcurrencySource: "default"}
	roles, _ := document["subagents"].(map[string]any)
	local, _ := document["local_acp"].(map[string]any)
	if controls.Legacy {
		result.EnabledSource = "legacy"
		if local["max_concurrent_subagents"] != nil {
			result.ConcurrencySource = "legacy"
		}
	} else {
		if roles["enabled"] != nil {
			result.EnabledSource = "yaml"
		}
		if roles["max_concurrent"] != nil {
			result.ConcurrencySource = "yaml"
		}
	}
	if explicit["disable-subagents"] {
		result.EnabledSource = "cli"
		if controls.Legacy && !controls.LegacyRoleEnabled {
			result.EnabledSource = "legacy_host_gate"
		}
	}
	return result
}
