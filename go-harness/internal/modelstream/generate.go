// Package modelstream collects complete model responses without using a
// non-streaming provider endpoint.
package modelstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	maxResponseBytes  = 16 << 20
	maxResponseChunks = 65536
)

// Generate is a synchronous result API, not a non-streaming transport. Keep
// reading through EOF, including usage-only trailers. On failure, cancel and
// drain before returning so callers can settle budgets after provider teardown.
func Generate(ctx context.Context, chat model.BaseModel[*schema.Message], input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return GenerateObserved(ctx, chat, input, nil, opts...)
}

// GenerateObserved also reports chunks drained after a failure so budget
// accounting retains any provider usage without returning partial content.
func GenerateObserved(ctx context.Context, chat model.BaseModel[*schema.Message], input []*schema.Message, observe func(*schema.Message) error, opts ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := chat.Stream(work, input, opts...)
	if stream == nil {
		if err == nil {
			err = errors.New("model returned nil stream")
		}
		return nil, err
	}
	defer stream.Close()
	if err != nil {
		cancel()
	}
	var chunks []*schema.Message
	bytes, count := 0, 0
	inconsistentUsage := false
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr == nil && observe != nil {
			recvErr = observe(chunk)
		}
		if err != nil {
			continue
		}
		if recvErr != nil {
			err = recvErr
		} else if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			count++
			var encoded []byte
			encoded, err = json.Marshal(chunk)
			bytes += len(encoded)
			if err == nil && (bytes > maxResponseBytes || count > maxResponseChunks) {
				err = errors.New("model stream response exceeds collection limit")
			}
		}
		if err != nil {
			cancel()
			chunks = nil
			continue
		}
		if chunk != nil {
			if meta := chunk.ResponseMeta; meta != nil && meta.Usage != nil {
				u := meta.Usage
				inconsistentUsage = inconsistentUsage || u.TotalTokens != 0 && u.TotalTokens != u.PromptTokens+u.CompletionTokens
			}
			chunks = append(chunks, chunk)
		}
	}
	if err != nil || ctx.Err() != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if len(chunks) == 0 {
		return nil, errors.New("model returned empty stream")
	}
	answer, err := schema.ConcatMessages(chunks)
	if err == nil && !inconsistentUsage && answer.ResponseMeta != nil && answer.ResponseMeta.Usage != nil {
		// Some providers send input usage at message_start and output usage at
		// message_delta. Concat takes maxima, so its total can miss one side.
		// Do not repair inconsistent counters that must stay conservatively billed.
		u := answer.ResponseMeta.Usage
		u.TotalTokens = max(u.TotalTokens, u.PromptTokens+u.CompletionTokens)
	}
	return answer, err
}
