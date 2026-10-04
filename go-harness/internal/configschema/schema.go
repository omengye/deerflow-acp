// Package configschema defines the settings contract shared by the native
// configuration tool and daemon. It performs no I/O and contains no secrets.
package configschema

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const Version = "deerflow-settings-v2"

// Field describes an editable draft field and its native YAML destination.
// Hash binds its validation and application semantics, not its current value.
type Field struct {
	Path          string         `json:"path"`
	ConfigPath    string         `json:"config_path"`
	Type          string         `json:"type"`
	Nullable      bool           `json:"nullable"`
	Default       any            `json:"default"`
	Enum          []string       `json:"enum,omitempty"`
	Minimum       *float64       `json:"minimum,omitempty"`
	Maximum       *float64       `json:"maximum,omitempty"`
	ZeroIsDefault bool           `json:"zero_is_default,omitempty"`
	MustBeTrue    bool           `json:"must_be_true,omitempty"`
	MaxLength     int            `json:"max_length,omitempty"`
	MaxBytes      int            `json:"max_bytes,omitempty"`
	MaxItems      int            `json:"max_items,omitempty"`
	ForTools      []string       `json:"for_tools,omitempty"`
	MaximumByTool map[string]int `json:"maximum_by_tool,omitempty"`
	Apply         string         `json:"apply"`
	Operations    []string       `json:"operations"`
	Hash          string         `json:"hash,omitempty"`
}

type Schema struct {
	Version      string        `json:"version"`
	Hash         string        `json:"hash"`
	Sections     []string      `json:"sections"`
	Fields       []Field       `json:"fields"`
	HostSettings []HostSetting `json:"host_settings,omitempty"`
}

// HostSetting advertises support without exposing a recipe or making it an
// editable desktop/model setting. Values remain only in the host YAML.
type HostSetting struct {
	ConfigPath  string `json:"config_path"`
	Apply       string `json:"apply"`
	Edit        string `json:"edit"`
	Description string `json:"description"`
}

// Descriptors cover only the explicitly named sections. Other sections must
// not infer support from this version or from an arbitrary JSON editor.
func Describe() Schema {
	fields := []Field{
		{Path: "/config_migration_ack", Type: "string", Default: ""},
		{Path: "/skills_enabled", Type: "boolean", Default: true},
		{Path: "/runtime/model_name", Type: "string", Nullable: true, Default: nil},
		numeric("runtime", "max_active_connections", "integer", 16, 1, 128),
		numeric("runtime", "max_active_runs", "integer", 2, 1, 128),
		numeric("runtime", "run_timeout_seconds", "number", 600, .001, 86400),
		numeric("runtime", "queue_timeout_seconds", "number", 600, .001, 86400),
		{Path: "/runtime/session_cleanup_enabled", Type: "boolean", Default: true},
		numeric("runtime", "inactive_session_retention_days", "integer", 30, 1, 3650),
		numeric("runtime", "closed_session_retention_days", "integer", 30, 0, 3650),
		numeric("runtime", "session_cleanup_interval_seconds", "number", 3600, 60, 86400),
		{Path: "/runtime/permission_mode", Type: "string", Default: "dangerous", Enum: []string{"off", "dangerous", "all"}},
		{Path: "/runtime/memory_scope", Type: "string", Default: "workspace", Enum: []string{"workspace", "session", "global"}},
		{Path: "/runtime/enable_bash", Type: "boolean", Default: false},
		{Path: "/runtime/tool_allowlist", Type: "string_array", Nullable: true, Default: nil},
		{Path: "/runtime/tool_denylist", Type: "string_array", Nullable: true, Default: []string{}},
		{Path: "/runtime/prompt_overlay", Type: "string", Default: "", MaxLength: 65536},
		{Path: "/memory/enabled", Type: "boolean", Default: true},
		{Path: "/memory/mode", Type: "string", Default: "middleware", Enum: []string{"middleware", "tool"}},
		{Path: "/memory/injection_enabled", Type: "boolean", Default: true},
		{Path: "/memory/retrieval_enabled", Type: "boolean", Default: true},
	}
	fields = append(fields, modelAgentFields()...)
	fields = append(fields, toolFields()...)
	fields = append(fields, evolutionFields()...)
	fields = append(fields, skillSourceFields()...)
	for i := range fields {
		f := &fields[i]
		f.ConfigPath = strings.TrimPrefix(strings.Replace(f.Path, "/runtime/", "/local_acp/", 1), "/")
		f.ConfigPath = strings.ReplaceAll(f.ConfigPath, "/", ".")
		if f.Path == "/skill_sources" || strings.HasPrefix(f.Path, "/skill_sources/") {
			f.ConfigPath = skillSourceConfigPath(f.Path)
		}
		if f.Path == "/skills_enabled" {
			f.ConfigPath = "skills.enabled"
		}
		if f.Path == "/models/*/clear_api_key" || f.Path == "/config_migration_ack" || f.Path == "/skill_evolution_migration_ack" || f.Path == "/tools/*/clear_api_key" || f.Path == "/tools/*/clear_https_proxy" {
			f.ConfigPath = "" // Credential edit intent, not a native YAML key.
		}
		f.Apply = "restart"
		f.Operations = []string{"validate", "save"}
		f.Hash = digest(*f)
	}
	schema := Schema{Version: Version, Sections: []string{"runtime", "memory", "models", "subagents", "tools", "sandbox", "skill_evolution", "skill_sources"}, Fields: fields}
	schema.HostSettings = []HostSetting{{ConfigPath: "mcp.host_recipes", Apply: "restart", Edit: "host_yaml", Description: "Explicit host-owned MCP recipes for exact workspaces; credentials are excluded from desktop and execution snapshots."}}
	schema.Hash = digest(schema)
	return schema
}

func numeric(section, key, kind string, value any, min, max float64) Field {
	return Field{Path: "/" + section + "/" + key, Type: kind, Default: value, Minimum: &min, Maximum: &max}
}

func digest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err) // Descriptors contain only fixed JSON values.
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func Defaults(section string) map[string]any {
	result := map[string]any{}
	for _, field := range Describe().Fields {
		if key, ok := strings.CutPrefix(field.Path, "/"+section+"/"); ok && !strings.Contains(key, "/") {
			result[key] = field.Default
		}
	}
	return result
}

func ValidateValue(path string, value any) error {
	for _, field := range Describe().Fields {
		if field.Path == path {
			return validate(field, value)
		}
	}
	return fmt.Errorf("%s: unsupported setting", path)
}

// ValidateDraft requires the complete supported projection. Unknown original
// YAML belongs to the saved file, never an arbitrary advanced draft editor.
func ValidateDraft(section string, document map[string]any) error {
	if section == "skill_sources" {
		return ValidateSkillSourcesDraft(document)
	}
	if section == "skill_evolution" {
		return ValidateEvolutionDraft(document, nil)
	}
	defaults := Defaults(section)
	if len(defaults) == 0 {
		return fmt.Errorf("%s: unsupported settings section", section)
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if err := ValidateValue("/"+section+"/"+key, document[key]); err != nil {
			return err
		}
	}
	for _, field := range Describe().Fields {
		if key, ok := strings.CutPrefix(field.Path, "/"+section+"/"); ok && !strings.Contains(key, "/") {
			if _, exists := document[key]; !exists {
				return fmt.Errorf("%s: missing setting", field.Path)
			}
		}
	}
	if section == "subagents" {
		return validateRoleMaps(document)
	}
	return nil
}

// ValidateNative checks only present native settings. Unknown retained fields
// remain migration input. YAML null means unset, as in the launch decoder.
func ValidateNative(document map[string]any) error {
	if _, err := HostMCPRecipes(document); err != nil {
		return err
	}
	if _, err := ProjectSubagentControls(document); err != nil {
		return err
	}
	if err := validateNativeTools(document); err != nil {
		return err
	}
	if err := validateNativeEvolution(document); err != nil {
		return err
	}
	if err := validateNativeSkillSources(document); err != nil {
		return err
	}
	for _, field := range Describe().Fields {
		if field.ConfigPath == "" || isEvolutionField(field) || field.Path == "/skill_sources" || strings.HasPrefix(field.Path, "/skill_sources/") {
			continue
		}
		section, key, _ := strings.Cut(field.ConfigPath, ".")
		values, _ := document[section].(map[string]any)
		value, exists := values[key]
		if !exists || value == nil {
			continue
		}
		// These native strings historically use empty to request defaults.
		if value == "" && (key == "permission_mode" || key == "memory_scope" || key == "mode") {
			continue
		}
		if err := validate(field, value); err != nil {
			return fmt.Errorf("%s: %w", field.ConfigPath, err)
		}
	}
	if models, ok := document["models"].([]any); ok {
		for index, item := range models {
			model, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("models.%d: must be an object", index)
			}
			for _, field := range Describe().Fields {
				key, applies := strings.CutPrefix(field.Path, "/models/*/")
				if !applies || key == "clear_api_key" {
					continue
				}
				value, exists := model[key]
				if !exists || value == nil {
					continue
				}
				if key == "provider" {
					if text, ok := value.(string); ok {
						native, err := harness.NativeModelProvider(text)
						if err != nil {
							return fmt.Errorf("models.%d.provider: unsupported provider", index)
						}
						value = native
					}
				}
				if err := validate(field, value); err != nil {
					return fmt.Errorf("models.%d: %w", index, err)
				}
			}
		}
	}
	if subagents, ok := document["subagents"].(map[string]any); ok {
		return validateRoleMaps(subagents)
	}
	return nil
}

func validate(field Field, value any) error {
	fail := func(reason string) error { return fmt.Errorf("%s: %s", field.Path, reason) }
	if value == nil {
		if field.Nullable {
			return nil
		}
		return fail("must not be null")
	}
	switch field.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fail("must be a boolean")
		}
		if field.MustBeTrue && value != true {
			return fail("must be true")
		}
	case "string":
		text, ok := value.(string)
		if !ok || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return fail("must be valid UTF-8 text without NUL")
		}
		if len(field.Enum) > 0 && !slices.Contains(field.Enum, text) {
			return fail("must be one of " + strings.Join(field.Enum, ", "))
		}
		if field.MaxLength > 0 && utf8.RuneCountInString(text) > field.MaxLength {
			return fail(fmt.Sprintf("must be at most %d characters", field.MaxLength))
		}
		if field.MaxBytes > 0 && len(text) > field.MaxBytes {
			return fail(fmt.Sprintf("must be at most %d UTF-8 bytes", field.MaxBytes))
		}
	case "integer", "number":
		var number float64
		switch v := value.(type) {
		case int:
			number = float64(v)
		case int64:
			number = float64(v)
		case uint64:
			number = float64(v)
		case float64:
			number = v
		case json.Number:
			var err error
			number, err = v.Float64()
			if err != nil {
				return fail("must be a number")
			}
		default:
			return fail("must be a number")
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || (field.Type == "integer" && number != math.Trunc(number)) {
			return fail("must be a finite " + field.Type)
		}
		if number == 0 && field.ZeroIsDefault {
			return nil
		}
		if number < *field.Minimum || number > *field.Maximum {
			if field.ZeroIsDefault {
				return fail(fmt.Sprintf("must be 0 (default) or %g..%g", *field.Minimum, *field.Maximum))
			}
			return fail(fmt.Sprintf("must be %g..%g", *field.Minimum, *field.Maximum))
		}
	case "string_array", "tool_names", "role_names":
		var values []string
		switch list := value.(type) {
		case []string:
			values = list
		case []any:
			for _, item := range list {
				text, ok := item.(string)
				if !ok {
					return fail("must contain only strings")
				}
				values = append(values, text)
			}
		default:
			return fail("must be an array of strings")
		}
		for _, text := range values {
			if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
				return fail("must contain valid UTF-8 names without NUL")
			}
		}
		if field.Type == "role_names" {
			if len(values) > 128 {
				return fail("must contain at most 128 names")
			}
			seen := map[string]bool{}
			for _, name := range values {
				if !harness.ValidAgentName(name) || seen[name] {
					return fail("contains an invalid or duplicate name")
				}
				seen[name] = true
			}
			return nil
		}
		if field.Type == "string_array" {
			values = NormalizeToolNames(values)
		}
		if err := (harness.ToolPolicy{Allowlist: values}).Validate(); err != nil {
			return fail(err.Error())
		}
	case "agent_map", "agent_role", "agent_model_settings":
		if _, ok := value.(map[string]any); !ok {
			return fail("must be a role object")
		}
	case "review_limits", "generation_settings", "discovery_settings", "behavior_settings", "auto_patch_settings", "maintenance_budget", "skill_source", "skill_source_settings":
		if _, ok := value.(map[string]any); !ok {
			return fail("must be an object")
		}
	case "opencli_sites":
		if err := validateOpenCLISites(value); err != nil {
			return fail(err.Error())
		}
	case "skill_source_catalog":
		items, ok := value.([]any)
		if !ok || len(items) > field.MaxItems {
			return fail(fmt.Sprintf("must be an array of at most %d sources", field.MaxItems))
		}
	case "model_catalog", "tool_catalog":
		if _, ok := value.([]any); !ok {
			return fail("must be a model array")
		}
	}
	return nil
}

// NormalizeToolNames preserves the native config's historical trimming and
// deduplication, including the distinction between nil and an explicit empty
// selection at the caller. Validation and consumption use the same result.
func NormalizeToolNames(values []string) []string {
	names := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// ValidateMemoryPolicy also applies after explicit launch overrides. A dormant
// legacy global scope may be retained, but cannot grant global memory access.
func ValidateMemoryPolicy(scope, mode string, enabled, retrieval bool) error {
	if enabled && scope == "global" {
		return fmt.Errorf("local_acp.memory_scope=global is not supported by the Go memory store")
	}
	if enabled && mode == "tool" && !retrieval {
		return fmt.Errorf("memory.mode=tool requires memory.retrieval_enabled=true")
	}
	return nil
}
