package tools

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

func SearchPreferenceTool(manager *websearch.Manager) (tool.BaseTool, error) {
	type input struct {
		Provider string `json:"provider" jsonschema:"description=brave/deepseek/searxng; only set a future default when explicitly requested by the user"`
	}
	return inferTool("set_search_provider", "Save the user's default search service for future tasks. Call only when the user explicitly asks to change the future default, such as 以后默认用 DeepSeek 搜索. A one-time provider belongs in web_search.provider instead. No credentials are accepted.", func(ctx context.Context, in input) (string, error) {
		status, err := manager.Select(ctx, in.Provider)
		var missing *websearch.ConfigurationError
		if _, interactive := interaction.UserInput(ctx); interactive && errors.As(err, &missing) {
			if _, err = manager.AwaitConfiguration(ctx, in.Provider); err == nil {
				status, err = manager.Select(ctx, in.Provider)
			}
		}
		if err != nil {
			return "", err
		}
		return marshalString(map[string]any{"default_provider": status.DefaultProvider, "saved": true, "effective": "new_search_tasks"})
	})
}
