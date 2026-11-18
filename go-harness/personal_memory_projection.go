package deerflow

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

// Revocation changes model input, never the immutable execution contract.
func currentPersonalMemory(ctx context.Context, store *memory.Store, cfg Config, req harness.RunRequest, pinned []memorySnapshot) ([]memorySnapshot, error) {
	current := slices.Clone(pinned)
	for i := range current {
		scope, err := memoryRunScope(cfg, req, current[i].Scope)
		if err != nil {
			return nil, err
		}
		current[i].Facts = make([]memorySnippet, 0, len(pinned[i].Facts))
		for _, snippet := range pinned[i].Facts {
			fact, err := store.Get(ctx, scope, snippet.ID)
			if errors.Is(err, harness.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if fact.Revision == snippet.Revision {
				snippet.Provenance = memory.Provenance(fact)
				current[i].Facts = append(current[i].Facts, snippet)
			}
		}
	}
	return current, nil
}

// Native checkpoints retain their system messages. Project the host's exact
// memory block at the model boundary too, without rewriting native history or
// touching user/tool messages. New facts remain available through memory tools.
type personalMemoryProjection struct {
	adk.BaseChatModelAgentMiddleware
	store            *memory.Store
	config           Config
	request          harness.RunRequest
	pinned, injected []memorySnapshot
}

func (p *personalMemoryProjection) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &personalMemoryModel{inner: inner, projection: p}, nil
}

type personalMemoryModel struct {
	inner      model.BaseModel[*schema.Message]
	projection *personalMemoryProjection
}

func (m *personalMemoryModel) input(ctx context.Context, input []*schema.Message) ([]*schema.Message, error) {
	p := m.projection
	current, err := currentPersonalMemory(ctx, p.store, p.config, p.request, p.pinned)
	if err != nil {
		return nil, err
	}
	replacement := memoryBlock(current)
	original, injected := memoryBlock(p.pinned), memoryBlock(p.injected)
	var projected []*schema.Message
	for i, message := range input {
		if message == nil || message.Role != schema.System {
			continue
		}
		content := message.Content
		for _, block := range []string{original, injected} {
			if block != "" && block != replacement {
				content = strings.ReplaceAll(content, block, replacement)
			}
		}
		if content != message.Content {
			if projected == nil {
				projected = slices.Clone(input)
			}
			copy := *message
			copy.Content = content
			projected[i] = &copy
		}
	}
	if projected != nil {
		return projected, nil
	}
	return input, nil
}

func (m *personalMemoryModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	input, err := m.input(ctx, input)
	if err != nil {
		return nil, err
	}
	return m.inner.Generate(ctx, input, opts...)
}

func (m *personalMemoryModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	input, err := m.input(ctx, input)
	if err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, input, opts...)
}
