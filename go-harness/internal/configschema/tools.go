package configschema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

var toolParameters = map[string][]string{
	"web_search":   {"api_key", "https_proxy", "timeout", "max_results"},
	"web_fetch":    {"https_proxy", "timeout", "max_output_chars"},
	"image_search": {"https_proxy", "timeout", "max_results"},
	"host_opencli": {"executable", "timeout", "allowed_sites"},
	"glob":         {"max_results"}, "grep": {"max_results"},
	"ls": {}, "read_file": {}, "write_file": {}, "str_replace": {},
	"move_path": {}, "delete_path": {}, "bash": {},
}

func toolFields() []Field {
	fields := []Field{
		{Path: "/tools", Type: "tool_catalog", Default: []any{}},
		{Path: "/tools/*/name", Type: "string", Enum: ToolNames(), Default: ""},
		{Path: "/tools/*/api_key", Type: "string", Default: ""},
		{Path: "/tools/*/clear_api_key", Type: "boolean", Default: false},
		{Path: "/tools/*/https_proxy", Type: "string", Default: ""},
		{Path: "/tools/*/clear_https_proxy", Type: "boolean", Default: false},
		numeric("tools/*", "timeout", "integer", 0, 0, 120),
		numeric("tools/*", "max_results", "integer", 0, 0, 1000),
		numeric("tools/*", "max_output_chars", "integer", 0, 0, 50000),
		{Path: "/tools/*/executable", Type: "string", Default: ""},
		{Path: "/tools/*/allowed_sites", Type: "opencli_sites", Nullable: true, Default: nil},
		{Path: "/sandbox/provider", Type: "string", Default: "local", Enum: []string{"local"}},
		{Path: "/sandbox/allow_host_bash", Type: "boolean", Default: false},
		{Path: "/sandbox/allow_host_tools", Type: "boolean", Default: false},
	}
	for i := range fields {
		field := &fields[i]
		key, tool := strings.CutPrefix(field.Path, "/tools/*/")
		if !tool {
			continue
		}
		for _, name := range ToolNames() {
			if key == "name" || slices.Contains(toolParameters[name], strings.TrimPrefix(key, "clear_")) {
				field.ForTools = append(field.ForTools, name)
			}
		}
		if key == "max_results" {
			field.MaximumByTool = map[string]int{"web_search": 20, "image_search": 50, "glob": 1000, "grep": 500}
		}
	}
	return fields
}

func ToolNames() []string {
	names := make([]string, 0, len(toolParameters))
	for name := range toolParameters {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ToolDefaults contains only fields with a native consumer for this tool.
// Zero numeric values select the tool's existing runtime defaults.
func ToolDefaults(name string) map[string]any {
	defaults := map[string]any{"name": name}
	for _, field := range toolFields() {
		if key, ok := strings.CutPrefix(field.Path, "/tools/*/"); ok && slices.Contains(field.ForTools, name) {
			defaults[key] = field.Default
		}
	}
	defaults["name"] = name
	return defaults
}

func ValidateToolDraft(tool map[string]any) error {
	name, _ := tool["name"].(string)
	if !slices.Contains(ToolNames(), name) {
		return fmt.Errorf("tools: unsupported tool name")
	}
	defaults := ToolDefaults(name)
	for key, value := range tool {
		if key == "original_name" {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("tools.%s.original_name: must be a string", name)
			}
			continue
		}
		if key == "api_key_configured" || key == "https_proxy_configured" {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("tools.%s.%s: must be a boolean", name, key)
			}
			continue
		}
		if _, ok := defaults[key]; !ok {
			return fmt.Errorf("tools.%s.%s: unsupported parameter", name, key)
		}
		if err := ValidateValue("/tools/*/"+key, value); err != nil {
			return fmt.Errorf("tools.%s: %w", name, err)
		}
	}
	// Sparse tool entries are valid; omitted fields request runtime defaults.
	return validateToolParameters(tool, name)
}

func validateToolParameters(raw map[string]any, name string) error {
	filtered := map[string]any{"name": name}
	for _, key := range toolParameters[name] {
		if value, exists := raw[key]; exists && value != nil {
			if err := ValidateValue("/tools/*/"+key, value); err != nil {
				return fmt.Errorf("tools.%s: %w", name, err)
			}
			filtered[key] = value
		}
	}
	encoded, err := yaml.Marshal(filtered)
	if err != nil {
		return fmt.Errorf("tools.%s: invalid parameters", name)
	}
	var config harness.BuiltinToolConfig
	if err := yaml.Unmarshal(encoded, &config); err != nil {
		return fmt.Errorf("tools.%s: invalid parameter types", name)
	}
	return config.Validate()
}

func validateNativeTools(document map[string]any) error {
	value := document["tools"]
	if value == nil {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("tools: must be an array")
	}
	seen := map[string]bool{}
	for index, item := range items {
		raw, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("tools.%d: must be an object", index)
		}
		name, _ := raw["name"].(string)
		if !slices.Contains(ToolNames(), name) || seen[name] {
			return fmt.Errorf("tools.%d.name: unsupported or duplicate tool", index)
		}
		seen[name] = true
		if err := validateToolParameters(raw, name); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenCLISites(value any) error {
	var sites []string
	switch values := value.(type) {
	case []string:
		sites = values
	case []any:
		for _, item := range values {
			site, ok := item.(string)
			if !ok {
				return fmt.Errorf("must contain only site names")
			}
			sites = append(sites, site)
		}
	default:
		return fmt.Errorf("must be an array of site names")
	}
	seen := map[string]bool{}
	if len(sites) > 64 {
		return fmt.Errorf("at most 64 site names")
	}
	for _, site := range sites {
		if !harness.ValidOpenCLIWord(site) || seen[site] {
			return fmt.Errorf("invalid or duplicate site name")
		}
		seen[site] = true
	}
	return nil
}
