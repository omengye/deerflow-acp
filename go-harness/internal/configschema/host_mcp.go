package configschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// HostMCPRecipes decodes host-only configuration. Unknown fields and coercions
// fail without echoing command arguments, environment, headers or URLs.
func HostMCPRecipes(document map[string]any) ([]harness.HostMCPRecipe, error) {
	raw, exists := document["mcp"]
	if !exists || raw == nil {
		return nil, nil
	}
	fail := func() ([]harness.HostMCPRecipe, error) {
		return nil, errors.New("mcp.host_recipes: invalid host-only recipe configuration; use the documented fields and types")
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return fail()
	}
	for key := range object {
		if key != "host_recipes" {
			return fail()
		}
	}
	if object["host_recipes"] == nil {
		return nil, nil
	}
	items, ok := object["host_recipes"].([]any)
	if !ok || len(items) > 32 {
		return fail()
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return fail()
		}
		if value, present := entry["restore_on_restart"]; present {
			if _, ok := value.(bool); !ok {
				return fail()
			}
		}
	}
	data, err := json.Marshal(object)
	if err != nil {
		return fail()
	}
	var target struct {
		Recipes []harness.HostMCPRecipe `json:"host_recipes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&target) != nil {
		return fail()
	}
	seen := map[string]bool{}
	for _, c := range target.Recipes {
		if c.ID == "" || len(c.ID) > 128 || strings.TrimSpace(c.ID) != c.ID || strings.IndexFunc(c.ID, unicode.IsControl) >= 0 || seen[c.ID] || !filepath.IsAbs(c.Workspace) || len(c.Servers) < 1 || len(c.Servers) > 8 {
			return fail()
		}
		seen[c.ID] = true
		names := map[string]bool{}
		for _, s := range c.Servers {
			if s.Name == "" || len(s.Name) > 128 || names[s.Name] || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 || len(s.Args) > 256 || len(s.Env) > 128 || len(s.Headers) > 64 {
				return fail()
			}
			names[s.Name] = true
			switch s.Transport {
			case "", "stdio":
				if !filepath.IsAbs(s.Command) || s.URL != "" || len(s.Headers) != 0 {
					return fail()
				}
			case "http", "sse":
				if s.URL == "" || s.Command != "" || len(s.Args) != 0 || len(s.Env) != 0 {
					return fail()
				}
			default:
				return fail()
			}
		}
	}
	return target.Recipes, nil
}
