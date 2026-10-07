package eino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
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

func TestCompactionSummarizesOlderExchangesWithinActiveTurn(t *testing.T) {
	original := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old request"), schema.AssistantMessage("old answer", nil), schema.UserMessage("current task")}
	chunk := strings.Repeat("read-only detail\n", 350)
	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		original = append(original, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_tool_output", Arguments: `{"id":"snapshot","offset":0}`}}}}, &schema.Message{Role: schema.Tool, ToolName: "read_tool_output", ToolCallID: id, Content: chunk})
	}
	last := original[len(original)-1]
	compact, err := finalizeCompaction(original, schema.AssistantMessage("Summarized the read-only findings.", nil), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(compact) >= len(original) || compact[2] != original[3] || compact[len(compact)-1] != last || original[5].Content != chunk {
		t.Fatal("active turn compaction changed original history, lost its request, or dropped the latest result")
	}
	unsafe := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old"), schema.AssistantMessage("old", nil), schema.UserMessage("current"), &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "write", Type: "function", Function: schema.FunctionCall{Name: "write_file"}}}}, &schema.Message{Role: schema.Tool, ToolName: "write_file", ToolCallID: "write", Content: strings.Repeat("effect evidence", 6000)}}
	if _, err := finalizeCompaction(unsafe, schema.AssistantMessage("summary", nil), 1); err == nil {
		t.Fatal("large write result was discarded as read-only evidence")
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
	if err != nil || got.ContextMessages != 0 || got.ContextTokens != 0 || got.KeepRecentMessages != 8 {
		t.Fatalf("defaults=%+v err=%v", got, err)
	}
}

func TestCompactionFirstTurnKeepsRequestAndParallelToolGroup(t *testing.T) {
	request := schema.UserMessage("Inspect the project and keep the requested changes.")
	original := []*schema.Message{schema.SystemMessage("policy"), request}
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		original = append(original, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_file"}}}}, &schema.Message{Role: schema.Tool, ToolCallID: id, ToolName: "read_file", Content: "earlier observations"})
	}
	parallel := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "write-a", Type: "function", Function: schema.FunctionCall{Name: "write_file"}}, {ID: "write-b", Type: "function", Function: schema.FunctionCall{Name: "write_file"}}}}
	first := &schema.Message{Role: schema.Tool, ToolCallID: "write-a", ToolName: "write_file", Content: "first write committed"}
	second := &schema.Message{Role: schema.Tool, ToolCallID: "write-b", ToolName: "write_file", Content: "second write committed"}
	original = append(original, parallel, first, second)
	after, err := finalizeCompaction(original, schema.AssistantMessage("Earlier reads summarized; two writes committed.", nil), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 6 || after[0] != original[0] || after[2] != request || after[3] != parallel || after[4] != first || after[5] != second {
		t.Fatalf("first turn or parallel tool group was lost: %+v", after)
	}
}

func TestNativeCompactionCanRepeatDuringFirstTask(t *testing.T) {
	const task = "Inspect all twelve sections and then answer the original request."
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	steps, summaries := 0, 0
	model := &scriptedModel{stream: func(_ context.Context, _ int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if len(messages) > 0 && (strings.Contains(messages[0].Content, "tasked with summarizing conversations") || strings.Contains(messages[0].Content, "负责总结对话")) {
			summaries++
			return textStream("The earlier sections have been inspected; continue the remaining sections."), nil
		}
		found := false
		for _, message := range messages {
			if message.Role == schema.User && message.Content == task {
				found = true
			}
		}
		if !found {
			t.Fatal("repeated compaction lost the original task")
		}
		if steps == 12 {
			return textStream("all sections inspected"), nil
		}
		steps++
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: string(rune('a' + steps)), Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
	}}
	engine := newTestEngine(t, Config{ChatModel: model, SessionStore: store, Tools: []tool.BaseTool{&recordingTool{}}, Budget: harness.DefaultBudgetLimits(), Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 6, ContextTokens: 32000, KeepRecentMessages: 2}})
	req := request("long-first-task")
	req.Input[0].Text = task
	result, err := engine.Run(context.Background(), req, nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || steps != 12 || summaries < 2 {
		t.Fatalf("first task did not survive repeated summaries: result=%+v steps=%d summaries=%d err=%v", result, steps, summaries, err)
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
