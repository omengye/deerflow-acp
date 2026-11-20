package launch

import (
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
)

func hostMCPRecipes(document map[string]any, values ConfigValues) ([]harness.HostMCPRecipe, error) {
	recipes, err := configschema.HostMCPRecipes(document)
	if err != nil {
		return nil, err
	}
	for i := range recipes {
		for j := range recipes[i].Servers {
			server := &recipes[i].Servers[j]
			for key, value := range server.Env {
				if server.Env[key], err = values.Resolve(value); err != nil {
					return nil, fmt.Errorf("mcp.host_recipes: an environment credential reference is unavailable: %w", err)
				}
			}
			for key, value := range server.Headers {
				if server.Headers[key], err = values.Resolve(value); err != nil {
					return nil, fmt.Errorf("mcp.host_recipes: a header credential reference is unavailable: %w", err)
				}
			}
			if server.URL, err = values.Resolve(server.URL); err != nil {
				return nil, fmt.Errorf("mcp.host_recipes: an endpoint reference is unavailable: %w", err)
			}
		}
	}
	return recipes, nil
}
