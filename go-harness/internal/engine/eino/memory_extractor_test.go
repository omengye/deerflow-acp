package eino

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMemoryStage3ExtractorRequiresTerminalUsage(t *testing.T) {
	content := func(text string) *schema.Message { return &schema.Message{Content: text} }
	usage := func(finish string, input, output int) *schema.Message {
		return &schema.Message{ResponseMeta: &schema.ResponseMeta{FinishReason: finish, Usage: &schema.TokenUsage{PromptTokens: input, CompletionTokens: output, TotalTokens: input + output}}}
	}
	stop := &schema.Message{ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}}
	for _, tc := range []struct {
		name     string
		parts    []*schema.Message
		complete bool
		reason   string
		tokens   int64
	}{
		{"usage_tail", []*schema.Message{content("{}"), stop, usage("", 10, 5)}, true, "", 15},
		{"usage_with_finish", []*schema.Message{content("{}"), usage("stop", 10, 5)}, true, "", 15},
		{"early_usage_without_finish", []*schema.Message{usage("", 10, 0), content("{}")}, false, "stream_incomplete", 10},
		{"early_usage_without_tail", []*schema.Message{usage("", 10, 0), content("{}"), stop}, false, "usage_missing", 10},
		{"missing_usage", []*schema.Message{content("{}"), stop}, false, "usage_missing", 0},
		{"oversize_still_reads_usage", []*schema.Message{content(strings.Repeat("x", 16385)), usage("length", 10, 100)}, true, "response_too_large", 110},
		{"invalid_usage", []*schema.Message{content("{}"), usage("stop", -1, 2)}, false, "invalid_usage", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				return schema.StreamReaderFromArray(tc.parts), nil
			}}
			e := &MemoryExtractor{chat: fake, identity: "fixture"}
			req, err := e.Prepare("extract", "我在北京")
			if err != nil {
				t.Fatal(err)
			}
			other, err := e.Prepare("extract", "我在上海")
			if err != nil {
				t.Fatal(err)
			}
			if req.Digest == other.Digest || req.InputTokens <= 0 {
				t.Fatal("request is not content-bound")
			}
			got := e.Stream(t.Context(), req, 100)
			if got.Complete != tc.complete || got.ErrorCode != tc.reason || got.Usage.TotalTokens != tc.tokens || len(got.Raw) > 16384 {
				t.Fatalf("%+v", got)
			}
		})
	}
}
