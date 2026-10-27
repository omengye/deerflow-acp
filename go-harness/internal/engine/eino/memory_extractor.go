package eino

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// NewMemoryExtractor uses the configured provider with no tools. The durable
// memory worker admits at most three 1024-token attempts per job, 200/day, and
// supplies a deadline. Streaming also works with stream-only provider gateways.
func NewMemoryExtractor(ctx context.Context, cfg Config) (func(context.Context, string, string) (string, error), error) {
	chat, err := newModelWithRetryPolicy(ctx, cfg, cfg.Model, true)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, instruction, input string) (string, error) {
		stream, err := chat.Stream(ctx, []*schema.Message{schema.SystemMessage(instruction), schema.UserMessage(input)}, model.WithMaxTokens(1024))
		if err != nil {
			return "", errors.New("memory model request failed")
		}
		defer stream.Close()
		var text strings.Builder
		for {
			part, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", errors.New("memory model stream failed")
			}
			if part != nil {
				text.WriteString(part.Content)
			}
			if text.Len() > 16384 {
				return "", errors.New("memory model response too large")
			}
		}
		if text.Len() == 0 {
			return "", errors.New("memory model returned no content")
		}
		return text.String(), nil
	}, nil
}
