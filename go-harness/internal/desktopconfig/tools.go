package desktopconfig

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"go.yaml.in/yaml/v3"
)

func toolDocuments(items []any) ([]any, []string, error) {
	output := make([]any, 0, len(items))
	diagnostics := []string{}
	seen := map[string]bool{}
	for index, item := range items {
		raw, ok := item.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("tools.%d: must be an object", index)
		}
		name := str(raw, "name", "")
		if !slices.Contains(configschema.ToolNames(), name) || seen[name] {
			return nil, nil, fmt.Errorf("tools.%d.name: unsupported or duplicate tool", index)
		}
		seen[name] = true
		tool := configschema.ToolDefaults(name)
		for key := range tool {
			if value, exists := raw[key]; exists && value != nil {
				tool[key] = value
			}
		}
		tool["original_name"] = name
		for _, key := range []string{"api_key", "https_proxy"} {
			if _, applies := tool[key]; !applies {
				continue
			}
			value := str(raw, key, "")
			tool[key+"_configured"] = value != ""
			if key == "api_key" && (!envReference.MatchString(strings.TrimSpace(value)) || strings.Contains(value, ":-")) || key == "https_proxy" && credentialProxy(value) {
				tool[key] = ""
			}
			tool["clear_"+key] = false
		}
		for key := range raw {
			if _, known := tool[key]; !known && key != "use" {
				diagnostics = append(diagnostics, fmt.Sprintf("tools.%s.%s：原数据保留，当前 Go 工具不读取此设置", name, key))
			}
		}
		output = append(output, tool)
	}
	sort.Strings(diagnostics)
	return output, diagnostics, nil
}

// Never project proxy userinfo or environment fallbacks that embed a credential.
func credentialProxy(value string) bool {
	if strings.Contains(value, "@") || strings.Contains(value, ":-") {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.User != nil
}

func validatedTools(incoming, previous []any) ([]any, error) {
	old := map[string]map[string]any{}
	for _, item := range previous {
		value, ok := item.(map[string]any)
		if !ok || old[str(value, "name", "")] != nil {
			return nil, fmt.Errorf("tools: invalid original tool list")
		}
		old[str(value, "name", "")] = value
	}
	output := make([]any, 0, len(incoming))
	seen := map[string]bool{}
	for _, item := range incoming {
		tool, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools: must contain objects")
		}
		if err := configschema.ValidateToolDraft(tool); err != nil {
			return nil, err
		}
		name := str(tool, "name", "")
		if seen[name] {
			return nil, fmt.Errorf("tools: duplicate tool %s", name)
		}
		seen[name] = true
		original := str(tool, "original_name", "")
		if original != "" && (original != name || old[original] == nil) {
			return nil, fmt.Errorf("tools.%s: original tool identity changed; reload", name)
		}
		prior := old[original]
		raw := map[string]any{}
		if prior != nil {
			if err := (harness.BuiltinToolConfig{Name: name, Use: str(prior, "use", "")}).Validate(); err != nil {
				return nil, err
			}
			raw = cloneMap(prior)
		}
		delete(raw, "use")
		for key, fallback := range configschema.ToolDefaults(name) {
			if strings.HasPrefix(key, "clear_") {
				continue
			}
			value, exists := tool[key]
			if !exists {
				value = fallback
			}
			if key == "api_key" || key == "https_proxy" {
				text, _ := value.(string)
				if text == redacted {
					return nil, fmt.Errorf("tools.%s.%s: re-enter the value", name, key)
				}
				if boolValue(tool, "clear_"+key, false) {
					delete(raw, key)
				} else if text != "" {
					raw[key] = text
				} else if prior == nil {
					delete(raw, key)
				}
				continue
			}
			raw[key] = value
		}
		// Validate restored secrets as well as the visible edit intent.
		check := configschema.ToolDefaults(name)
		for key := range check {
			if value, exists := raw[key]; exists {
				check[key] = value
			}
		}
		if err := configschema.ValidateToolDraft(check); err != nil {
			return nil, err
		}
		// Retained historical fields still pass through the native YAML decoder.
		// Refuse a save that the daemon would reject after merging those fields.
		encoded, err := yaml.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("tools.%s: invalid native configuration", name)
		}
		var setting harness.BuiltinToolConfig
		if err := yaml.Unmarshal(encoded, &setting); err != nil {
			return nil, fmt.Errorf("tools.%s: invalid native parameter types", name)
		}
		if err := setting.Validate(); err != nil {
			return nil, err
		}
		output = append(output, raw)
	}
	return output, nil
}
