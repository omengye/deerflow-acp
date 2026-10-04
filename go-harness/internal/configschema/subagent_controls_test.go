package configschema

import (
	"fmt"
	"testing"
)

func TestSubagentControlsPreserveLegacyGatesUntilExplicitFormatMigration(t *testing.T) {
	for _, local := range []any{nil, false, true} {
		for _, role := range []any{nil, false, true} {
			t.Run(fmt.Sprintf("local=%v/role=%v", local, role), func(t *testing.T) {
				doc := map[string]any{"config_version": 16, "local_acp": map[string]any{"subagent_enabled": local, "max_concurrent_subagents": 3}, "subagents": map[string]any{"enabled": role}}
				controls, err := ProjectSubagentControls(doc)
				if err != nil {
					t.Fatal(err)
				}
				want := local == true && role != false
				if !controls.Legacy || controls.Enabled != want || controls.MaxConcurrent != 3 {
					t.Fatalf("projection %+v", controls)
				}
				if controls.ResolveEnabled(false, true) != want || controls.ResolveEnabled(true, true) || controls.ResolveEnabled(true, false) != (role != false) {
					t.Fatal("legacy CLI/gate precedence changed")
				}
				native, err := ProjectSubagentControls(map[string]any{"config_version": NativeConfigVersion, "subagents": map[string]any{"enabled": controls.Enabled, "max_concurrent": controls.MaxConcurrent}})
				if err != nil || native.Enabled != want || native.MaxConcurrent != 3 {
					t.Fatalf("migration changed default: %+v %v", native, err)
				}
				if native.ResolveEnabled(true, true) || !native.ResolveEnabled(true, false) {
					t.Fatal("explicit CLI did not win in native format")
				}
			})
		}
	}
}

func TestSubagentControlsRejectAmbiguousOrMalformedConfiguration(t *testing.T) {
	for _, doc := range []map[string]any{
		{"subagents": "legacy"}, {"local_acp": []any{}},
		{"config_version": "future"}, {"config_version": 17}, {"config_version": 1.5},
		{"config_version": NativeConfigVersion, "local_acp": map[string]any{"subagent_enabled": nil}},
		{"config_version": NativeConfigVersion, "local_acp": map[string]any{"max_concurrent_subagents": 2}},
		{"subagents": map[string]any{"max_concurrent": 2}},
		{"local_acp": map[string]any{"subagent_enabled": "false"}},
		{"local_acp": map[string]any{"max_concurrent_subagents": 1.5}},
		{"config_version": NativeConfigVersion, "subagents": map[string]any{"max_concurrent": 65}},
	} {
		if _, err := ProjectSubagentControls(doc); err == nil {
			t.Fatalf("invalid controls accepted: %+v", doc)
		}
	}
}
