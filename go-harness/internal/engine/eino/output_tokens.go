package eino

import (
	"context"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstream"
)

// outputTokenModel is the last boundary before provider I/O, after every host,
// role and context-window reservation. All callers keep using WithMaxTokens as
// the single provider-neutral cap, including probes and maintenance workers.
// Generate also uses streaming transport; no provider may opt into a separate
// non-streaming endpoint for synchronous auxiliary calls.
type outputTokenModel struct {
	inner     model.ToolCallingChatModel
	provider  string
	parameter harness.OutputTokenParameter
}

func (m *outputTokenModel) options(opts []model.Option) []model.Option {
	if m.provider == "claude" || m.provider == "anthropic" || m.provider == "injected" {
		return opts
	}
	common := model.GetCommonOptions(nil, opts...)
	if common.MaxTokens == nil {
		return opts
	}
	result := append([]model.Option(nil), opts...)
	cap := *common.MaxTokens
	if m.parameter == harness.OutputTokensCompletion {
		// Keep only one wire cap while preserving unrelated model options.
		result = nil
		for _, opt := range opts {
			if model.GetCommonOptions(nil, opt).MaxTokens == nil {
				result = append(result, opt)
			}
		}
	} else {
		cap = 0
	}
	result = append(result, openai.WithMaxCompletionTokens(cap))
	return result
}

func (m *outputTokenModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return modelstream.Generate(ctx, m, input, opts...)
}

func (m *outputTokenModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.provider == "" || m.provider == "openai" || m.provider == "openai-compatible" {
		input = openAIToolImageMessages(input)
	}
	stream, err := m.inner.Stream(ctx, input, m.options(opts)...)
	return stream, classifyModelContextError(err)
}

func (m *outputTokenModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &outputTokenModel{inner: inner, provider: m.provider, parameter: m.parameter}, nil
}

func (m *outputTokenModel) GetType() string {
	if typed, ok := m.inner.(interface{ GetType() string }); ok {
		return typed.GetType()
	}
	return m.provider
}

func (m *outputTokenModel) IsCallbacksEnabled() bool {
	if callbacks, ok := m.inner.(interface{ IsCallbacksEnabled() bool }); ok {
		return callbacks.IsCallbacksEnabled()
	}
	return false
}
