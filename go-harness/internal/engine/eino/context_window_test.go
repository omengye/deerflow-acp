package eino

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestProviderContextLimitClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		context bool
	}{
		{"local", harness.ErrContextLimit, true},
		{"code", &openaiapi.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded", Message: "private diagnostic"}, true},
		{"gemini", &openaiapi.APIError{HTTPStatusCode: 400, Message: "The input token count exceeds the maximum number of tokens allowed (1048576)."}, true},
		{"openai", &openaiapi.APIError{HTTPStatusCode: 400, Message: "This model's maximum context length is 8192 tokens."}, true},
		{"other_400", &openaiapi.APIError{HTTPStatusCode: 400, Message: "Invalid tool schema"}, false},
		{"rate_limit", &openaiapi.APIError{HTTPStatusCode: 429, Code: "context_length_exceeded"}, false},
		{"untyped", errors.New("The input token count exceeds the maximum number of tokens allowed"), false},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, role := range []bool{false, true} {
				got := (&trackedModel{role: role}).modelError(fmt.Errorf("provider: %w", tc.err))
				if errors.Is(got, harness.ErrContextLimit) != tc.context || !errors.Is(got, tc.err) {
					t.Fatalf("role=%t classification=%v", role, got)
				}
			}
		})
	}
	if got := classifyModelContextError(nil); got != nil {
		t.Fatalf("nil error became %v", got)
	}
}

func TestProviderContextLimitHTTPIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"The input token count exceeds the maximum number of tokens allowed (1048576).","type":"invalid_request_error"}}`)
	}))
	defer server.Close()
	e := newTestEngine(t, Config{Provider: "openai", Model: "test-model", BaseURL: server.URL, APIKey: "fixture"})
	_, err := e.Run(context.Background(), request("provider-window"), nil, nil)
	if !errors.Is(err, harness.ErrContextLimit) || calls.Load() != 1 {
		t.Fatalf("provider calls=%d error=%v", calls.Load(), err)
	}
}

func TestContextWindowRejectsBeforeProviderAdmission(t *testing.T) {
	fake := &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("unexpected"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "test-model", ContextWindows: map[string]int{"test-model": 512}})
	req := request("oversized-window")
	req.Input[0].Text = strings.Repeat("数据库迁移", 300)
	_, err := e.Run(context.Background(), req, nil, nil)
	if !errors.Is(err, harness.ErrContextLimit) || fake.calls != 0 {
		t.Fatalf("err=%v provider calls=%d", err, fake.calls)
	}
}

func TestContextWindowContractsAndUnicodeEstimate(t *testing.T) {
	c, err := normalizeCompaction(harness.CompactionConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	c = windowCompaction(c, 32768, 4096)
	if c.ContextTokens > 24000 || c.ContextTokens < 20000 {
		t.Fatalf("unsafe threshold %d", c.ContextTokens)
	}
	if estimatedTextTokens([]byte(strings.Repeat("中", 100))) <= estimatedTextTokens([]byte(strings.Repeat("x", 100))) {
		t.Fatal("Chinese text is underestimated")
	}
	e := newTestEngine(t, Config{ChatModel: &scriptedModel{}, ContextWindows: map[string]int{"test-model": 32768}})
	first, err := e.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.config.ContextWindows["test-model"] = 8192
	second, err := e.executionContract(nil, nil)
	if err != nil || first == second {
		t.Fatal("changed window did not invalidate execution policy", err)
	}
}

func TestAutomaticCompactionTracksSelectedModelWindow(t *testing.T) {
	for _, tc := range []struct {
		window, explicit, want int
	}{
		{0, 0, 32000},
		{32768, 0, 21012},
		{128000, 0, 91008},
		{1048576, 0, 767631},
		{128000, 30000, 30000},
		{32768, 100000, 21012},
		{0, 100000, 100000},
	} {
		c := windowCompaction(harness.CompactionConfig{Enabled: true, ContextTokens: tc.explicit}, tc.window, 4096)
		if c.ContextTokens != tc.want || c.ContextMessages != 0 {
			t.Fatalf("window=%d explicit=%d: threshold=%d want=%d messages=%d", tc.window, tc.explicit, c.ContextTokens, tc.want, c.ContextMessages)
		}
	}
}

func TestLargeWindowDoesNotUseFallbackCompactionThreshold(t *testing.T) {
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if strings.Contains(input[0].Content, "tasked with summarizing conversations") || strings.Contains(input[0].Content, "负责总结对话") {
			t.Fatal("large model compacted at the unknown-window fallback")
		}
		return textStream("answer"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "fixture", SessionStore: store, ContextWindows: map[string]int{"fixture": 128000}, Compaction: harness.CompactionConfig{Enabled: true, KeepRecentMessages: 1}})
	for i, prompt := range []string{"short first request", strings.Repeat("x", 150000)} {
		req := request(string(rune('a' + i)))
		req.Input[0].Text = prompt
		result, err := e.Run(context.Background(), req, nil, nil)
		if err != nil || result.StopReason != "end_turn" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if fake.calls != 2 {
		t.Fatalf("unexpected summary calls: %d", fake.calls)
	}
}

func TestFreshRequestAboveCompactionThresholdStillFitsWindow(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if strings.Contains(input[0].Content, "tasked with summarizing conversations") || strings.Contains(input[0].Content, "负责总结对话") {
			t.Fatal("fresh user request was summarized without older exchanges")
		}
		return textStream("answer"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "fixture", ContextWindows: map[string]int{"fixture": 32768}, Compaction: harness.CompactionConfig{Enabled: true}})
	req := request("large-fresh-task")
	req.Input[0].Text = strings.Repeat("x", 100000)
	result, err := e.Run(context.Background(), req, nil, nil)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 1 {
		t.Fatalf("fresh request was rejected by compaction: result=%+v calls=%d err=%v", result, fake.calls, err)
	}
}
