package eino

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/network"
)

// ProbeModel uses the same provider constructors as normal Go harness runs.
// Callers must sanitize provider errors because HTTP clients can include
// request headers and credentials in their error text.
func ProbeModel(ctx context.Context, provider, name, baseURL, apiKey string, parameter ...harness.OutputTokenParameter) error {
	return ProbeModelWithNetwork(ctx, provider, name, baseURL, apiKey, harness.NetworkConfig{}, parameter...)
}

func ProbeModelWithNetwork(ctx context.Context, provider, name, baseURL, apiKey string, policy harness.NetworkConfig, parameter ...harness.OutputTokenParameter) error {
	cfg := Config{Provider: provider, Model: name, BaseURL: baseURL, APIKey: apiKey, Network: policy}
	if len(parameter) > 0 {
		cfg.OutputTokenParameter = parameter[0]
	}
	return ProbeModelWithConfig(ctx, cfg)
}

func ProbeModelWithConfig(ctx context.Context, cfg Config) error {
	chat, err := newModel(ctx, cfg, cfg.Model)
	if err != nil {
		return err
	}
	_, err = chat.Generate(ctx, []*schema.Message{schema.UserMessage("Reply OK.")}, model.WithMaxTokens(512))
	return err
}

func newModel(ctx context.Context, cfg Config, name string) (model.ToolCallingChatModel, error) {
	// Tracked model calls own bounded retries and meter every HTTP attempt.
	return newModelWithRetryPolicy(ctx, cfg, name, true)
}

// Maintenance workers own their bounded, persisted retries. Provider SDK
// retries must not create extra requests under one admitted model call.
func newModelWithRetryPolicy(ctx context.Context, cfg Config, name string, disableRetries bool) (model.ToolCallingChatModel, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("model is required")
	}
	if err := harness.ValidateModelAPIOptions(cfg.Provider, cfg.API, cfg.OutputTokenParameter); err != nil {
		return nil, err
	}
	api, err := harness.ResolveModelAPI(cfg.Provider, cfg.API)
	if err != nil {
		return nil, err
	}
	parameter, err := harness.ResolveOutputTokenParameter(cfg.Provider, cfg.BaseURL, cfg.OutputTokenParameter)
	if err != nil {
		return nil, err
	}
	wrap := func(inner model.ToolCallingChatModel, err error) (model.ToolCallingChatModel, error) {
		if err != nil {
			return nil, err
		}
		return &outputTokenModel{inner: inner, provider: strings.ToLower(cfg.Provider), parameter: parameter, responses: api == harness.ModelAPIResponses}, nil
	}
	var client *http.Client
	if cfg.Network.Proxy.Enabled {
		client, err = network.NewClient(cfg.Network, "", true)
		if err != nil {
			return nil, err
		}
	}
	switch strings.ToLower(cfg.Provider) {
	case "", "openai", "openai-compatible":
		if api == harness.ModelAPIResponses {
			return wrap(newResponsesModel(ctx, cfg, name, client))
		}
		return wrap(openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: name, HTTPClient: client}))
	case "claude", "anthropic":
		conf := &claude.Config{APIKey: cfg.APIKey, Model: name, MaxTokens: 8192, DisableRetries: disableRetries}
		if client != nil {
			// Preserve the Anthropic SDK's response-header deadline without
			// introducing a total timeout that would truncate long streams.
			client.Transport.(*http.Transport).ResponseHeaderTimeout = 10 * time.Minute
			conf.HTTPClient = client
		}
		if cfg.BaseURL != "" {
			conf.BaseURL = &cfg.BaseURL
		}
		return wrap(claude.NewChatModel(ctx, conf))
	default:
		return nil, fmt.Errorf("unsupported model provider %q", cfg.Provider)
	}
}
