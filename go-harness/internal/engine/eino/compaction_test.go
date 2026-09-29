package eino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCompactionFinalizeKeepsActiveTurnAndToolPair(t *testing.T) {
	oldSkill := &schema.Message{Role: schema.Tool, ToolName: "skill", ToolCallID: "skill-1", Content: "Pinned research instructions"}
	call := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tool-1", Type: "function", Function: schema.FunctionCall{Name: "read_workspace"}}}}
	result := &schema.Message{Role: schema.Tool, ToolCallID: "tool-1", Content: "file contents"}
	original := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old request"), oldSkill, schema.AssistantMessage("old answer", nil), schema.UserMessage("recent request"), call, result, schema.AssistantMessage("recent answer", nil), schema.UserMessage("active request")}
	after, err := finalizeCompaction(original, schema.AssistantMessage("Summary of old work", nil), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 8 || after[0].Role != schema.System || !strings.Contains(after[1].Content, "Summary of old work") || !strings.Contains(after[2].Content, "Pinned research instructions") || after[3] != original[4] || after[4] != call || after[5] != result || after[7] != original[8] {
		t.Fatalf("unsafe compacted context: %+v", after)
	}
	if _, err := finalizeCompaction(original, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "bad"}}}, 3); err == nil {
		t.Fatal("tool call was accepted as a summary")
	}
	if _, err := finalizeCompaction(original, schema.AssistantMessage(strings.Repeat("x", maxSummaryBytes+1), nil), 3); err == nil {
		t.Fatal("oversized summary was accepted")
	}
}

func TestCompactionConfigRequiresRoomForRecentWindow(t *testing.T) {
	for _, bad := range []harness.CompactionConfig{
		{Enabled: true, ContextMessages: 5, KeepRecentMessages: 4},
		{Enabled: true, ContextMessages: -1},
		{Enabled: true, ContextTokens: -1},
	} {
		if _, err := normalizeCompaction(bad); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	got, err := normalizeCompaction(harness.CompactionConfig{Enabled: true})
	if err != nil || got.ContextMessages <= got.KeepRecentMessages+1 || got.ContextTokens == 0 {
		t.Fatalf("defaults=%+v err=%v", got, err)
	}
}

func TestNativeCompactionPersistsReplacementAndUsesSharedBudget(t *testing.T) {
	dir := t.TempDir()
	store, err := einosession.NewFileStore[*schema.Message](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 2 {
			return textStream("Old work was about the first two requests."), nil
		}
		return textStream("answer"), nil
	}}
	engine := newTestEngine(t, Config{ChatModel: model, SessionStore: store, Budget: harness.BudgetLimits{MaxModelCalls: 2, MaxTokens: 100000, MaxOutputTokens: 1024}, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
	for i, prompt := range []string{"first request", "second request", "third request"} {
		req := request(string(rune('a' + i)))
		req.Input[0].Text = prompt
		result, err := engine.Run(context.Background(), req, nil, nil)
		if err != nil || result.StopReason != "end_turn" {
			t.Fatalf("turn %d: result=%+v err=%v", i, result, err)
		}
	}
	if model.calls != 4 {
		t.Fatalf("summary did not share the third turn's two-call budget: %d", model.calls)
	}
	events, err := store.LoadEvents(context.Background(), "test-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events.Events {
		if event.Kind == adk.SessionEventMessagesReplaced {
			found = true
			if event.MessagesReplaced == nil || len(*event.MessagesReplaced) == 0 {
				t.Fatal("empty native replacement")
			}
		}
	}
	if !found {
		t.Fatal("native session replacement was not persisted")
	}
	seen := model.seen[3]
	var joined strings.Builder
	for _, message := range seen {
		joined.WriteString(message.Content)
	}
	if !strings.Contains(joined.String(), "Old work was about") || !strings.Contains(joined.String(), "third request") || strings.Contains(joined.String(), "first request") {
		t.Fatalf("model received unexpected compacted history: %s", joined.String())
	}
	reopened, err := einosession.NewFileStore[*schema.Message](dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newTestEngine(t, Config{ChatModel: model, SessionStore: reopened, Budget: harness.BudgetLimits{MaxModelCalls: 2, MaxTokens: 100000, MaxOutputTokens: 1024}, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
	fourth := request("d")
	fourth.Input[0].Text = "fourth request"
	if _, err := restarted.Run(context.Background(), fourth, nil, nil); err != nil {
		t.Fatal(err)
	}
	joined.Reset()
	for _, message := range model.seen[4] {
		joined.WriteString(message.Content)
	}
	if !strings.Contains(joined.String(), "Old work was about") || !strings.Contains(joined.String(), "fourth request") || strings.Contains(joined.String(), "first request") {
		t.Fatalf("restarted model did not receive persisted summary: %s", joined.String())
	}
}

func TestCompactionFailurePreservesNativeHistory(t *testing.T) {
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	model := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 2 {
			return nil, errors.New("summary unavailable")
		}
		return textStream("answer"), nil
	}}
	engine := newTestEngine(t, Config{ChatModel: model, SessionStore: store, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 4, ContextTokens: 100000, KeepRecentMessages: 1}})
	for i := 0; i < 2; i++ {
		if _, err := engine.Run(context.Background(), request(string(rune('a'+i))), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Run(context.Background(), request("c"), nil, nil); err == nil || !strings.Contains(err.Error(), "summary unavailable") {
		t.Fatalf("summary failure was hidden: %v", err)
	}
	events, err := store.LoadEvents(context.Background(), "test-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Events {
		if event.Kind == adk.SessionEventMessagesReplaced {
			t.Fatal("failed summary replaced native history")
		}
	}
}
