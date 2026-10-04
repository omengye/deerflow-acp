package eino

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ark"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ProbeModel uses the same provider constructors as normal Go harness runs.
// Callers must sanitize provider errors because HTTP clients can include
// request headers and credentials in their error text.
func ProbeModel(ctx context.Context, provider, name, baseURL, apiKey string) error {
	chat, err := newModel(ctx, Config{Provider: provider, Model: name, BaseURL: baseURL, APIKey: apiKey}, name)
	if err != nil {
		return err
	}
	_, err = chat.Generate(ctx, []*schema.Message{schema.UserMessage("Reply OK.")})
	return err
}

func newModel(ctx context.Context, cfg Config, name string) (model.ToolCallingChatModel, error) {
	return newModelWithRetryPolicy(ctx, cfg, name, false)
}

// Maintenance workers own their bounded, persisted retries. Provider SDK
// retries must not create extra requests under one admitted model call.
func newModelWithRetryPolicy(ctx context.Context, cfg Config, name string, disableRetries bool) (model.ToolCallingChatModel, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("model is required")
	}
	switch strings.ToLower(cfg.Provider) {
	case "", "openai", "openai-compatible":
		return openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name})
	case "claude", "anthropic":
		conf := &claude.Config{APIKey: cfg.APIKey, Model: name, MaxTokens: 8192, DisableRetries: disableRetries}
		if cfg.BaseURL != "" {
			conf.BaseURL = &cfg.BaseURL
		}
		return claude.NewChatModel(ctx, conf)
	case "ark":
		conf := &ark.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name}
		if disableRetries {
			zero := 0
			conf.RetryTimes = &zero
		}
		return ark.NewChatModel(ctx, conf)
	default:
		return nil, fmt.Errorf("unsupported model provider %q", cfg.Provider)
	}
}
