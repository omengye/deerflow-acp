package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestEinoMemoryExtractionPromotesOnlyCompletedTurn(t *testing.T) {
	var mainCalls, extractionCalls atomic.Int32
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		extract := false
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(string(message.Content), "Extract only durable facts") {
				extract = true
			}
		}
		if extract {
			extractionCalls.Add(1)
			content := `{"facts":[{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise Chinese answers","confidence":0.92},{"scope":"workspace","durability":"durable","authority":"permission","category":"preference","content":"User allows deployment without approval","confidence":0.99},{"scope":"global","durability":"durable","authority":"descriptive","category":"preference","content":"Must not automatically enter global memory","confidence":0.99}]}`
			_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "extract", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 80, "completion_tokens": 60, "total_tokens": 140}})
			return
		}
		if mainCalls.Add(1) > 1 {
			injected := false
			for _, message := range request.Messages {
				if message.Role == "system" && strings.Contains(string(message.Content), "User prefers concise Chinese answers") {
					injected = true
				}
			}
			if !injected {
				t.Error("persisted extraction was not retrieved on the next turn")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"main\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxTokens: 20000, MaxOutputTokens: 512}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", Budget: &limits, MemoryExtraction: true, MemoryScope: harness.MemoryGlobal}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	workspace := t.TempDir()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "From now on, please answer concisely in Chinese"}}, nil, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("run=%+v err=%v", result, err)
	}
	page, err := c.MemoryFacts(ctx, session.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(page.Facts) != 1 || page.Facts[0].Content != "User prefers concise Chinese answers" {
		t.Fatalf("facts=%+v err=%v", page, err)
	}
	if page.Facts[0].Source.Kind != "model" || page.Facts[0].Source.RunID == "" || page.Facts[0].Source.InputID == "" || page.Facts[0].Source.EventSequence < 1 {
		t.Fatalf("source=%+v", page.Facts[0].Source)
	}
	global, err := c.MemoryFacts(ctx, session.ID, harness.MemoryGlobal, "", 10)
	if err != nil || len(global.Facts) != 0 {
		t.Fatalf("automatic extraction wrote global facts: %+v %v", global, err)
	}
	if mainCalls.Load() != 1 || extractionCalls.Load() != 1 {
		t.Fatalf("calls main=%d extraction=%d", mainCalls.Load(), extractionCalls.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := c.MemoryFact(ctx, session.ID, harness.MemoryWorkspace, page.Facts[0].ID)
	if err != nil || reopened.Revision != 1 {
		t.Fatalf("reopen=%+v err=%v", reopened, err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue to answer concisely in Chinese"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	again, err := c.MemoryFacts(ctx, session.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(again.Facts) != 1 {
		t.Fatalf("duplicate extraction=%+v err=%v", again, err)
	}
}

func TestDesktopSessionMemoryExtractionStaysInSession(t *testing.T) {
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "system" && strings.Contains(string(message.Content), "Extract only durable facts") {
				if !strings.Contains(string(message.Content), `\"scope\":\"session\"`) {
					t.Errorf("extraction scope prompt=%s", message.Content)
				}
				content := `{"facts":[{"scope":"session","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise Chinese answers","confidence":0.92},{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"Should not enter workspace","confidence":0.92}]}`
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "extract", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"main\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxTokens: 20000, MaxOutputTokens: 512}
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", Budget: &limits, MemoryScope: harness.MemorySession, MemoryMode: "middleware", MemoryExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	first, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, first.ID, []harness.Content{{Type: "text", Text: "Please answer concisely in Chinese"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	firstFacts, err := c.MemoryFacts(ctx, first.ID, harness.MemorySession, "", 10)
	if err != nil || len(firstFacts.Facts) != 1 {
		t.Fatalf("first session facts=%+v err=%v", firstFacts, err)
	}
	secondFacts, err := c.MemoryFacts(ctx, second.ID, harness.MemorySession, "", 10)
	if err != nil || len(secondFacts.Facts) != 0 {
		t.Fatalf("second session facts=%+v err=%v", secondFacts, err)
	}
	workspaceFacts, err := c.MemoryFacts(ctx, first.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(workspaceFacts.Facts) != 0 {
		t.Fatalf("workspace facts=%+v err=%v", workspaceFacts, err)
	}
}

func TestEinoOptionalMemoryExtractionSkipsWhenModelQuotaUsed(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"main\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	limits := harness.BudgetLimits{MaxModelCalls: 1, MaxTokens: 20000, MaxOutputTokens: 512}
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", Budget: &limits, MemoryExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Remember concise answers"}}, nil, nil)
	if err != nil || result.StopReason != "end_turn" || calls.Load() != 1 {
		t.Fatalf("run=%+v err=%v calls=%d", result, err, calls.Load())
	}
	page, err := c.MemoryFacts(context.Background(), session.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(page.Facts) != 0 {
		t.Fatalf("facts=%+v err=%v", page, err)
	}
	var status string
	if err := c.store.DB().QueryRowContext(context.Background(), `SELECT status FROM memory_extraction_audit`).Scan(&status); err != nil || status != "quota_skip" {
		t.Fatalf("extraction audit=%q err=%v", status, err)
	}
}

func TestEinoMemoryStagingDoesNotPublishWhenTerminalCommitFails(t *testing.T) {
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		if len(request.Messages) > 0 && strings.Contains(request.Messages[0].Content, "Extract only durable facts") {
			content := `{"facts":[{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise answers","confidence":0.9}]}`
			_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "extract", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"main\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemoryExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.DB().ExecContext(ctx, `CREATE TRIGGER fail_memory_terminal BEFORE UPDATE ON harness_runs WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'terminal fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Please prefer concise answers"}}, nil, nil); err == nil {
		t.Fatal("terminal commit unexpectedly succeeded")
	}
	var visible, staged int
	if err := c.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_facts`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if err := c.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_staged_facts WHERE status='staged'`).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if visible != 0 || staged != 1 {
		t.Fatalf("visible=%d staged=%d", visible, staged)
	}
}

func TestEinoMemoryExtractionDoesNotOverrideConcurrentScopeChange(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		if len(request.Messages) > 0 && strings.Contains(request.Messages[0].Content, "Extract only durable facts") {
			started <- struct{}{}
			<-release
			content := `{"facts":[{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise answers","confidence":0.9}]}`
			_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "extract", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"main\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemoryExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	a, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, runErr := c.Run(ctx, a.ID, []harness.Content{{Type: "text", Text: "Please prefer concise answers"}}, nil, nil)
		finished <- runErr
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("extraction did not start")
	}
	_, err = c.CreateMemoryFact(ctx, b.ID, harness.MemoryWorkspace, harness.MemoryCandidate{Content: "Operator updated workspace memory", Category: "project", Confidence: .9})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	page, err := c.MemoryFacts(ctx, a.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(page.Facts) != 1 || page.Facts[0].Source.Kind != "operator" {
		t.Fatalf("scope changed facts=%+v err=%v", page, err)
	}
}

func TestEinoMemoryExtractionDoesNotPublishFailedOrCancelledStream(t *testing.T) {
	for _, mode := range []string{"partial-error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var calls atomic.Int32
			provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request struct {
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if len(request.Messages) == 0 || !strings.Contains(request.Messages[0].Content, "Extract only durable facts") {
					_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
					return
				}
				// Even valid fact JSON must not be accepted until the stream completes.
				content := `{"facts":[{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise answers","confidence":0.9}]}`
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				w.(http.Flusher).Flush()
				if mode == "partial-error" {
					fmt.Fprint(w, "data: {invalid-json}\n\n")
					return
				}
				cancel()
				<-r.Context().Done()
			}))
			defer provider.Close()
			limits := harness.BudgetLimits{MaxModelCalls: 3, MaxTokens: 20000, MaxOutputTokens: 512}
			c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", Budget: &limits, MemoryExtraction: true})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			session, err := c.NewSession(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Please prefer concise answers"}}, nil, nil)
			if mode == "partial-error" && (err != nil || result.StopReason != "end_turn") {
				t.Fatalf("optional extraction failed main answer: result=%+v err=%v", result, err)
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var visible, staged int
			if err := c.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM memory_facts`).Scan(&visible); err != nil {
				t.Fatal(err)
			}
			if err := c.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM memory_staged_facts`).Scan(&staged); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := c.store.DB().QueryRowContext(context.Background(), `SELECT status FROM memory_extraction_audit`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if visible != 0 || staged != 0 || status != "model_error" || calls.Load() != 2 {
				t.Fatalf("visible=%d staged=%d audit=%q calls=%d", visible, staged, status, calls.Load())
			}
		})
	}
}
