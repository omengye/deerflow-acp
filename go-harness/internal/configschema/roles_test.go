package configschema

import (
	"strings"
	"testing"
)

func TestRoleDefaultsStaySparseAndNestedRulesReachNative(t *testing.T) {
	doc := Defaults("subagents")
	for key := range doc {
		if strings.Contains(key, "/") {
			t.Fatalf("nested default leaked into section: %s", key)
		}
	}
	if len(doc["agents"].(map[string]any)) != 0 {
		t.Fatal("builtin overrides materialized")
	}
	if err := ValidateDraft("subagents", doc); err != nil {
		t.Fatal(err)
	}
	role := map[string]any{"description": "审查", "system_prompt": "使用证据", "model": nil, "tools": []any{}, "skills": nil, "thinking_enabled": nil, "model_settings": map[string]any{"temperature": 0, "max_tokens": nil}, "timeout_seconds": .001}
	doc["custom_agents"] = map[string]any{"reviewer": role}
	if err := ValidateDraft("subagents", doc); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNative(map[string]any{"config_version": NativeConfigVersion, "subagents": doc}); err != nil {
		t.Fatal(err)
	}
	role["model_settings"].(map[string]any)["max_tokens"] = 1.5
	for _, err := range []error{ValidateDraft("subagents", doc), ValidateNative(map[string]any{"config_version": NativeConfigVersion, "subagents": doc})} {
		if err == nil || !strings.Contains(err.Error(), "/subagents/custom_agents/reviewer/model_settings/max_tokens") {
			t.Fatalf("missing concrete field error: %v", err)
		}
	}
}
