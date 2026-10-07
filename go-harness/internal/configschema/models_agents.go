package configschema

import (
	"fmt"
	"strings"
)

func modelAgentFields() []Field {
	fields := []Field{
		{Path: "/default_model", Type: "string", Default: ""},
		{Path: "/models", Type: "model_catalog", Default: []any{}},
		{Path: "/models/*/name", Type: "string", Default: "", MaxLength: 256},
		{Path: "/models/*/display_name", Type: "string", Default: "", MaxLength: 4096},
		{Path: "/models/*/description", Type: "string", Default: "", MaxLength: 4096},
		{Path: "/models/*/provider", Type: "string", Default: "openai", Enum: []string{"openai", "ark", "claude"}},
		{Path: "/models/*/model", Type: "string", Default: "", MaxLength: 256},
		{Path: "/models/*/api_key", Type: "string", Default: ""},
		{Path: "/models/*/base_url", Type: "string", Default: ""},
		{Path: "/models/*/supports_vision", Type: "boolean", Default: false},
		numeric("models/*", "context_window", "integer", 0, 0, 1<<30),
		{Path: "/models/*/clear_api_key", Type: "boolean", Default: false},
		{Path: "/subagents/enabled", Type: "boolean", Default: false},
		numeric("subagents", "max_concurrent", "integer", 2, 1, 64),
		{Path: "/subagents/auto_continue", Type: "boolean", Default: false},
		{Path: "/subagents/agents", Type: "agent_map", Default: map[string]any{}},
		{Path: "/subagents/custom_agents", Type: "agent_map", Default: map[string]any{}},
	}
	for _, spec := range []struct {
		key, kind string
		min, max  float64
	}{
		{"timeout_seconds", "number", .001, 86400},
		{"max_turns", "integer", 0, 10000},
		{"max_depth", "integer", 1, 3},
		{"background_workers", "integer", 1, 64},
		{"max_tasks_per_root", "integer", 1, 10000},
		{"max_auto_continuations", "integer", 0, 1000},
	} {
		field := numeric("subagents", spec.key, spec.kind, nil, spec.min, spec.max)
		field.Nullable = true
		fields = append(fields, field)
	}
	fields = append(fields, roleFields()...)
	return fields
}

func roleFields() []Field {
	var fields []Field
	for _, collection := range []string{"agents", "custom_agents"} {
		root := "/subagents/" + collection + "/*"
		fields = append(fields,
			Field{Path: root, Type: "agent_role", Default: map[string]any{}},
			Field{Path: root + "/model_settings", Type: "agent_model_settings", Nullable: true, Default: nil},
			Field{Path: root + "/description", Type: "string", Nullable: true, Default: nil, MaxBytes: 4096},
			Field{Path: root + "/system_prompt", Type: "string", Nullable: true, Default: nil, MaxBytes: 65536},
			Field{Path: root + "/model", Type: "string", Nullable: true, Default: nil, MaxBytes: 256},
			Field{Path: root + "/thinking_enabled", Type: "boolean", Nullable: true, Default: nil},
			Field{Path: root + "/reasoning_effort", Type: "string", Nullable: true, Default: nil, Enum: []string{"", "low", "medium", "high", "xhigh"}},
		)
		for _, key := range []string{"tools", "disallowed_tools", "skills", "delegate_to"} {
			kind := "tool_names"
			if key == "skills" || key == "delegate_to" {
				kind = "role_names"
			}
			fields = append(fields, Field{Path: root + "/" + key, Type: kind, Nullable: true, Default: nil})
		}
		for _, spec := range []struct {
			key, kind string
			min, max  float64
		}{
			{"timeout_seconds", "number", .001, 86400},
			{"max_turns", "integer", 0, 10000},
			{"model_settings/temperature", "number", 0, 2},
			{"model_settings/max_tokens", "integer", 1, 1 << 30},
			{"model_settings/thinking_budget_tokens", "integer", 1024, (1 << 30) - 1},
		} {
			field := numeric(strings.TrimPrefix(root, "/"), spec.key, spec.kind, nil, spec.min, spec.max)
			field.Nullable = true
			fields = append(fields, field)
		}
	}
	return fields
}

// Role definitions are sparse overlays: omitted/null scalars remain unset and
// nil/empty grants keep their native meanings. Check all present nested keys;
// graph, builtin, model reference and provider checks remain in agents.DecodeConfig.
func validateRoleMaps(document map[string]any) error {
	definitions := map[string]Field{}
	for _, field := range roleFields() {
		definitions[field.Path] = field
	}
	var check func(string, string, any) error
	check = func(schemaPath, actualPath string, value any) error {
		field, ok := definitions[schemaPath]
		if !ok {
			return fmt.Errorf("%s: unsupported role setting", actualPath)
		}
		field.Path = actualPath
		if err := validate(field, value); err != nil {
			return err
		}
		if object, ok := value.(map[string]any); ok {
			for key, child := range object {
				if err := check(schemaPath+"/"+key, actualPath+"/"+key, child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, collection := range []string{"agents", "custom_agents"} {
		roles, _ := document[collection].(map[string]any)
		for name, role := range roles {
			prefix := "/subagents/" + collection + "/"
			if err := check(prefix+"*", prefix+name, role); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateModelDraft covers only native model options and credential-edit
// metadata. Opaque provider extensions are retained in the original file, never
// exposed through an unrestricted draft object.
func ValidateModelDraft(model map[string]any) error {
	metadata := map[string]string{"original_name": "string", "api_key_configured": "boolean", "api_key_literal": "boolean"}
	for key, value := range model {
		if kind, ok := metadata[key]; ok {
			if err := validate(Field{Path: "/models/*/" + key, Type: kind}, value); err != nil {
				return err
			}
			continue
		}
		if err := ValidateValue("/models/*/"+key, value); err != nil {
			return err
		}
	}
	for _, field := range Describe().Fields {
		if key, ok := strings.CutPrefix(field.Path, "/models/*/"); ok {
			if _, exists := model[key]; !exists {
				return fmt.Errorf("%s: missing setting", field.Path)
			}
		}
	}
	for _, key := range []string{"name", "model"} {
		if value, _ := model[key].(string); strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("/models/*/%s: must be a nonempty single-line identifier", key)
		}
	}
	return nil
}
