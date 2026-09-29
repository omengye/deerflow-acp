package eino

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ark"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

func newModel(ctx context.Context, cfg Config, name string) (model.ToolCallingChatModel, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("model is required")
	}
	switch strings.ToLower(cfg.Provider) {
	case "", "openai", "openai-compatible":
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name})
	case "claude", "anthropic":
		conf := &claude.Config{APIKey: cfg.APIKey, Model: name, MaxTokens: 8192}
		if cfg.BaseURL != "" {
			conf.BaseURL = &cfg.BaseURL
		}
		return claude.NewChatModel(ctx, conf)
	case "ark":
		return ark.NewChatModel(ctx, &ark.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name})
	default:
		return nil, fmt.Errorf("unsupported model provider %q", cfg.Provider)
	}
}
