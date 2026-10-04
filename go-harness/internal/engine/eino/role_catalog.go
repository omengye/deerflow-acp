package eino

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

type roleCatalogKey struct{}
type pinnedRoleCatalog struct{ catalog *agents.Catalog }

func (e *Engine) RoleCatalog() (*agents.Catalog, error) { return agents.Snapshot(e.config.Subagents) }

// WithRoleCatalog reconstructs definitions while retaining current host tool,
// provider, permission and budget policy. Changing those constraints still
// invalidates the original native execution contract.
func (e *Engine) WithRoleCatalog(catalog *agents.Catalog) (*Engine, error) {
	pinned := *e
	if catalog == nil {
		pinned.config.Subagents, pinned.roles = nil, nil
		return &pinned, nil
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(catalog.Config)
	if err != nil {
		return nil, err
	}
	var config harness.SubagentsConfig
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	providers := map[string]string{e.config.Model: e.config.Provider}
	for _, choice := range e.config.Models {
		providers[choice.Value] = e.config.Provider
	}
	if e.config.ChatModel != nil {
		providers[e.config.Model] = "injected"
	}
	for name, route := range e.config.ModelRoutes {
		providers[name] = route.Provider
	}
	models := make(map[string]bool, len(providers))
	for name := range providers {
		models[name] = true
	}
	pinned.roles, err = agents.New(config, models)
	if err != nil {
		return nil, fmt.Errorf("restore role catalog: %w", err)
	}
	if err = config.ValidateModelOptions(providers, e.config.Budget.MaxOutputTokens); err != nil {
		return nil, err
	}
	pinned.config.Subagents = &config
	if pinned.config.Delegations == nil {
		pinned.config.Delegations, err = agents.NewController(nil)
		if err != nil {
			return nil, err
		}
	}
	return &pinned, nil
}

// EngineForRoleContext lets host extension factories use the same pinned
// catalog as the Eino parent. It never reads role definitions from tool input.
func (e *Engine) EngineForRoleContext(ctx context.Context) (*Engine, error) {
	if pinned, ok := ctx.Value(roleCatalogKey{}).(pinnedRoleCatalog); ok {
		return e.WithRoleCatalog(pinned.catalog)
	}
	return e, nil
}
