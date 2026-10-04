package launch

import (
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
)

func hostMCPRecipes(document map[string]any) ([]harness.HostMCPRecipe, error) {
	recipes, err := configschema.HostMCPRecipes(document)
	if err != nil {
		return nil, err
	}
	for i := range recipes {
		for j := range recipes[i].Servers {
			server := &recipes[i].Servers[j]
			for key, value := range server.Env {
				if server.Env[key], err = resolveConfigValue(value); err != nil {
					return nil, errors.New("mcp.host_recipes: an environment credential reference is unavailable")
				}
			}
			for key, value := range server.Headers {
				if server.Headers[key], err = resolveConfigValue(value); err != nil {
					return nil, errors.New("mcp.host_recipes: a header credential reference is unavailable")
				}
			}
			if server.URL, err = resolveConfigValue(server.URL); err != nil {
				return nil, errors.New("mcp.host_recipes: an endpoint reference is unavailable")
			}
		}
	}
	return recipes, nil
}
