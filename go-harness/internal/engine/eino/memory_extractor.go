package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

type MemoryExtractor struct {
	chat     model.ToolCallingChatModel
	identity string
}
type MemoryRequest struct {
	messages    []*schema.Message
	Digest      string
	InputTokens int64
}

// Provider retries are disabled. The worker must reserve and dispatch every
// request, and durably save its response/usage before closing the attempt.
func NewMemoryExtractor(ctx context.Context, cfg Config) (*MemoryExtractor, error) {
	chat, err := newModelWithRetryPolicy(ctx, cfg, cfg.Model, true)
	if err != nil {
		return nil, err
	}
	return &MemoryExtractor{chat: chat, identity: cfg.Provider + "/" + string(cfg.API) + "/" + cfg.Model + "/" + cfg.BaseURL}, nil
}
func (e *MemoryExtractor) Prepare(instruction, input string) (MemoryRequest, error) {
	req := MemoryRequest{messages: []*schema.Message{schema.SystemMessage(instruction), schema.UserMessage(input)}}
	raw, err := json.Marshal(req.messages)
	if err != nil {
		return req, err
	}
	req.Digest = fmt.Sprintf("%x", sha256.Sum256(append([]byte(e.identity+"\x00"), raw...)))
	req.InputTokens, err = estimateModelTokens(req.messages, nil)
	return req, err
}
func (e *MemoryExtractor) Stream(ctx context.Context, req MemoryRequest, outputCap int) memory.JobResponse {
	result := memory.JobResponse{}
	stream, err := e.chat.Stream(ctx, req.messages, model.WithMaxTokens(outputCap))
	if err != nil {
		result.ErrorCode = "model_request_failed"
		return result
	}
	defer stream.Close()
	var text strings.Builder
	known, terminal, oversize := false, false, false
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			result.Raw = text.String()
			result.Complete = known && terminal
			if !terminal {
				result.ErrorCode = "stream_incomplete"
			} else if !known {
				result.ErrorCode = "usage_missing"
			} else if oversize {
				result.ErrorCode = "response_too_large"
			}
			return result
		}
		if err != nil {
			result.Raw = text.String()
			result.ErrorCode = "model_stream_failed"
			return result
		}
		if part == nil {
			continue
		}
		if text.Len()+len(part.Content) > 16384 {
			oversize = true
		} else if !oversize {
			text.WriteString(part.Content)
		}
		// Usage may arrive after the final content/finish_reason. Always drain EOF.
		if part.ResponseMeta != nil && part.ResponseMeta.FinishReason != "" {
			terminal = true
		}
		if part.ResponseMeta != nil && part.ResponseMeta.Usage != nil {
			u := part.ResponseMeta.Usage
			if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 {
				result.ErrorCode = "invalid_usage"
				return result
			}
			known = known || terminal
			result.Usage = harness.Usage{InputTokens: max(result.Usage.InputTokens, int64(u.PromptTokens)), OutputTokens: max(result.Usage.OutputTokens, int64(u.CompletionTokens)), TotalTokens: max(result.Usage.TotalTokens, int64(u.TotalTokens))}
		}
	}
}
