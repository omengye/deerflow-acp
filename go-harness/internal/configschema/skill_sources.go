package configschema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var skillSourceKeys = []string{"id", "root", "scope", "workspace"}
var skillSourceConfigKeys = map[string]string{"import_path": "path", "export_root": "export_root", "include_global": "include_global", "managed": "managed_sources"}

func skillSourceFields() []Field {
	return []Field{
		{Path: "/skill_sources", Type: "skill_source_settings", Default: map[string]any{}},
		{Path: "/skill_sources/import_path", Type: "string", Default: "", MaxLength: 8192},
		{Path: "/skill_sources/export_root", Type: "string", Default: "", MaxLength: 8192},
		{Path: "/skill_sources/include_global", Type: "boolean", Nullable: true, Default: nil},
		{Path: "/skill_sources/managed", Type: "skill_source_catalog", Default: []any{}, MaxItems: 32},
		{Path: "/skill_sources/managed/*", Type: "skill_source", Default: map[string]any{}},
		{Path: "/skill_sources/managed/*/id", Type: "string", Default: "", MaxLength: 64},
		{Path: "/skill_sources/managed/*/root", Type: "string", Default: "", MaxLength: 8192},
		{Path: "/skill_sources/managed/*/scope", Type: "string", Default: "workspace", Enum: []string{"workspace", "global"}},
		{Path: "/skill_sources/managed/*/workspace", Type: "string", Default: "", MaxLength: 8192},
	}
}

func skillSourceConfigPath(path string) string {
	if path == "/skill_sources" {
		return "skills"
	}
	suffix := strings.TrimPrefix(path, "/skill_sources/")
	key, rest, nested := strings.Cut(suffix, "/")
	native := "skills." + skillSourceConfigKeys[key]
	if nested {
		native += "." + strings.ReplaceAll(rest, "/", ".")
	}
	return native
}

// ProjectSkillSources produces a complete editable projection without reading
// directories or copying unrecognized legacy values into the desktop.
func ProjectSkillSources(raw map[string]any) (map[string]any, []string, error) {
	result := Defaults("skill_sources")
	for key, native := range skillSourceConfigKeys {
		if raw[native] != nil {
			result[key] = raw[native]
		}
	}
	if err := validateSourceTopLevel(result); err != nil {
		return nil, nil, err
	}
	items := result["managed"].([]any)
	projected := make([]any, 0, len(items))
	for i, item := range items {
		values, ok := item.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("skills.managed_sources.%d: must be an object", i)
		}
		entry := map[string]any{"id": "", "root": "", "scope": "workspace", "workspace": ""}
		for key, value := range values {
			if !slices.Contains(skillSourceKeys, key) {
				return nil, nil, fmt.Errorf("skills.managed_sources.%d: unsupported field; expected id, root, scope, workspace", i)
			}
			if value != nil {
				entry[key] = value
			}
		}
		// Scope must be explicit in native YAML. A missing scope remains an
		// invalid value that the user can repair; it never silently grants scope.
		if values["scope"] == nil {
			entry["scope"] = ""
		}
		for _, key := range skillSourceKeys {
			if _, ok := entry[key].(string); !ok {
				return nil, nil, fmt.Errorf("skills.managed_sources.%d.%s: must be a string", i, key)
			}
		}
		projected = append(projected, entry)
	}
	result["managed"] = projected
	var ignored []string
	for key := range raw {
		if !slices.Contains([]string{"enabled", "path", "extensions_file", "managed_sources", "export_root", "include_global"}, key) {
			ignored = append(ignored, "skills."+key)
		}
	}
	slices.Sort(ignored)
	return result, ignored, nil
}

func validateSourceTopLevel(document map[string]any) error {
	for key := range document {
		if _, known := skillSourceConfigKeys[key]; !known {
			return fmt.Errorf("/skill_sources/%s: unsupported setting", key)
		}
	}
	for key := range skillSourceConfigKeys {
		value, present := document[key]
		if !present {
			return fmt.Errorf("/skill_sources/%s: missing setting", key)
		}
		if err := ValidateValue("/skill_sources/"+key, value); err != nil {
			return err
		}
	}
	return nil
}

func ValidateSkillSourcesDraft(document map[string]any) error {
	if err := validateSourceTopLevel(document); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, item := range document["managed"].([]any) {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("/skill_sources/managed/%d: must be an object", i)
		}
		for key := range entry {
			if !slices.Contains(skillSourceKeys, key) {
				return fmt.Errorf("/skill_sources/managed/%d: unsupported source field", i)
			}
		}
		for _, key := range skillSourceKeys {
			if err := ValidateValue("/skill_sources/managed/*/"+key, entry[key]); err != nil {
				return fmt.Errorf("/skill_sources/managed/%d/%s: invalid source value", i, key)
			}
		}
		id := entry["id"].(string)
		if !harness.ValidSkillName(id) || seen[id] || id == "python-public" || id == "python-custom" {
			return fmt.Errorf("/skill_sources/managed/%d/id: requires a unique non-reserved skill source name", i)
		}
		seen[id] = true
		if entry["root"] == "" {
			return fmt.Errorf("/skill_sources/managed/%d/root: directory is required", i)
		}
		if entry["scope"] == "workspace" && entry["workspace"] == "" {
			return fmt.Errorf("/skill_sources/managed/%d/workspace: directory is required for workspace scope", i)
		}
		if entry["scope"] == "global" && entry["workspace"] != "" {
			return fmt.Errorf("/skill_sources/managed/%d/workspace: must be empty for global scope", i)
		}
	}
	if len(seen) > 0 && document["export_root"] == "" {
		return fmt.Errorf("/skill_sources/export_root: required for managed sources")
	}
	return nil
}

// MergeSkillSources leaves unchanged original representations and legacy data
// intact. Only an explicit edit replaces a source definition or clears a path.
func MergeSkillSources(document, previous map[string]any) (map[string]any, error) {
	original, _, err := ProjectSkillSources(previous)
	if err != nil {
		return nil, err
	}
	if err := ValidateSkillSourcesDraft(document); err != nil {
		if previous["enabled"] != false || !SameValue(document, original) {
			return nil, err
		}
	}
	result := make(map[string]any, len(previous))
	for key, value := range previous {
		result[key] = value
	}
	for key, native := range skillSourceConfigKeys {
		value := document[key]
		if SameValue(value, original[key]) {
			continue
		}
		if value == nil || (key != "managed" && value == "") {
			delete(result, native)
		} else {
			result[native] = value
		}
	}
	return result, nil
}

func validateNativeSkillSources(document map[string]any) error {
	raw, ok := document["skills"].(map[string]any)
	if !ok {
		if document["skills"] != nil {
			return fmt.Errorf("skills: must be an object")
		}
		return nil
	}
	if raw["enabled"] == false {
		return nil
	}
	projection, _, err := ProjectSkillSources(raw)
	if err != nil {
		return err
	}
	if err := ValidateSkillSourcesDraft(projection); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	return nil
}
