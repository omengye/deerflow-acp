package configschema

import (
	"encoding/json"
	"fmt"
	"math"
)

// NativeConfigVersion opts into the single subagent gate and native concurrent
// limit. Legacy numeric versions keep their original two-gate CLI semantics
// until an explicit, backed-up settings migration.
const NativeConfigVersion = "eino-harness-v1"

type SubagentControls struct {
	Legacy             bool `json:"migration_required"`
	Enabled            bool `json:"enabled"`
	MaxConcurrent      int  `json:"max_concurrent"`
	LegacyLocalEnabled bool `json:"legacy_local_enabled"`
	LegacyRoleEnabled  bool `json:"legacy_role_enabled"`
}

func NativeFormat(document map[string]any) (bool, error) {
	value := document["config_version"]
	if value == NativeConfigVersion {
		return true, nil
	}
	if value == nil {
		return false, nil
	}
	var version float64
	switch value := value.(type) {
	case int:
		version = float64(value)
	case int64:
		version = float64(value)
	case float64:
		version = value
	case json.Number:
		version, _ = value.Float64()
	default:
		return false, fmt.Errorf("config_version: unsupported configuration format; expected %s or a legacy numeric version", NativeConfigVersion)
	}
	if math.IsNaN(version) || version != math.Trunc(version) || version < 1 || version > 16 {
		return false, fmt.Errorf("config_version: unknown legacy version; explicit migration is required")
	}
	return false, nil
}

// ProjectSubagentControls reads configuration only. It never writes files or
// changes an active daemon; the desktop must distinguish this projection from
// effective CLI/host policy. Invalid legacy values cannot silently become defaults.
func ProjectSubagentControls(document map[string]any) (SubagentControls, error) {
	for _, section := range []string{"local_acp", "subagents"} {
		if value := document[section]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				return SubagentControls{}, fmt.Errorf("%s: must be an object", section)
			}
		}
	}
	native, err := NativeFormat(document)
	if err != nil {
		return SubagentControls{}, err
	}
	result := SubagentControls{Legacy: !native, MaxConcurrent: 2, LegacyRoleEnabled: true}
	local, _ := document["local_acp"].(map[string]any)
	roles, _ := document["subagents"].(map[string]any)
	boolean := func(values map[string]any, key, path string, fallback bool) (bool, error) {
		value := values[key]
		if value == nil {
			return fallback, nil
		}
		if err := validate(Field{Path: path, Type: "boolean"}, value); err != nil {
			return false, err
		}
		return value.(bool), nil
	}
	if native {
		for _, key := range []string{"subagent_enabled", "max_concurrent_subagents"} {
			if _, exists := local[key]; exists {
				return result, fmt.Errorf("local_acp.%s: removed in %s; use subagents.enabled/max_concurrent", key, NativeConfigVersion)
			}
		}
		result.Enabled, err = boolean(roles, "enabled", "/subagents/enabled", false)
	} else {
		if _, exists := roles["max_concurrent"]; exists {
			return result, fmt.Errorf("subagents.max_concurrent requires config_version: %s; migrate the legacy controls first", NativeConfigVersion)
		}
		result.LegacyLocalEnabled, err = boolean(local, "subagent_enabled", "/local_acp/subagent_enabled", false)
		if err == nil {
			result.LegacyRoleEnabled, err = boolean(roles, "enabled", "/subagents/enabled", true)
		}
		result.Enabled = result.LegacyLocalEnabled && result.LegacyRoleEnabled
	}
	if err != nil {
		return result, err
	}
	value, path := roles["max_concurrent"], "/subagents/max_concurrent"
	if !native {
		value, path = local["max_concurrent_subagents"], "/local_acp/max_concurrent_subagents"
	}
	if value != nil {
		field := numeric("subagents", "max_concurrent", "integer", 2, 1, 64)
		field.Path = path
		if err := validate(field, value); err != nil {
			return result, err
		}
		switch value := value.(type) {
		case int:
			result.MaxConcurrent = value
		case int64:
			result.MaxConcurrent = int(value)
		case uint64:
			result.MaxConcurrent = int(value)
		case float64:
			result.MaxConcurrent = int(value)
		case json.Number:
			number, _ := value.Float64()
			result.MaxConcurrent = int(number)
		}
	}
	return result, nil
}

// ResolveEnabled preserves old precedence while native files use explicit CLI
// selection first. Session/model settings still cannot override this host result.
func (c SubagentControls) ResolveEnabled(explicitDisable, disable bool) bool {
	if !explicitDisable {
		return c.Enabled
	}
	if c.Legacy && !c.LegacyRoleEnabled {
		return false
	}
	return !disable
}
