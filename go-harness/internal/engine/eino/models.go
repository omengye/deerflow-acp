package eino

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ProbeModel uses the same provider constructors as normal Go harness runs.
// Callers must sanitize provider errors because HTTP clients can include
// request headers and credentials in their error text.
func ProbeModel(ctx context.Context, provider, name, baseURL, apiKey string, parameter ...harness.OutputTokenParameter) error {
	cfg := Config{Provider: provider, Model: name, BaseURL: baseURL, APIKey: apiKey}
	if len(parameter) > 0 {
		cfg.OutputTokenParameter = parameter[0]
	}
	chat, err := newModel(ctx, cfg, name)
	if err != nil {
		return err
	}
	_, err = chat.Generate(ctx, []*schema.Message{schema.UserMessage("Reply OK.")}, model.WithMaxTokens(512))
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
	parameter, err := harness.ResolveOutputTokenParameter(cfg.Provider, cfg.BaseURL, cfg.OutputTokenParameter)
	if err != nil {
		return nil, err
	}
	wrap := func(inner model.ToolCallingChatModel, err error) (model.ToolCallingChatModel, error) {
		if err != nil {
			return nil, err
		}
		return &outputTokenModel{inner: inner, provider: strings.ToLower(cfg.Provider), parameter: parameter}, nil
	}
	switch strings.ToLower(cfg.Provider) {
	case "", "openai", "openai-compatible":
		return wrap(openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name}))
	case "claude", "anthropic":
		conf := &claude.Config{APIKey: cfg.APIKey, Model: name, MaxTokens: 8192, DisableRetries: disableRetries}
		if cfg.BaseURL != "" {
			conf.BaseURL = &cfg.BaseURL
		}
		return wrap(claude.NewChatModel(ctx, conf))
	default:
		return nil, fmt.Errorf("unsupported model provider %q", cfg.Provider)
	}
}
