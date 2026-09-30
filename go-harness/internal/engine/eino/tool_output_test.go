package eino

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

type capturedToolOutputs struct {
	owner  harness.Session
	text   string
	reads  int
	stored int
}

func (s *capturedToolOutputs) StoreToolOutput(_ context.Context, x harness.Session, output string) (harness.AssetRef, error) {
	s.owner, s.text = x, output
	s.stored++
	return harness.AssetRef{ID: "large-result", SessionID: x.ID, Size: int64(len(output)), Kind: harness.AssetToolOutput}, nil
}

func TestManyLargeToolResultsStayWithinCurrentTurnCompactionWindow(t *testing.T) {
	outputs := &capturedToolOutputs{}
	mw := &toolMiddleware{outputs: outputs, sink: &eventSink{request: harness.RunRequest{Session: harness.Session{ID: "owner", CWD: t.TempDir()}}}}
	messages := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old request"), schema.AssistantMessage("old answer", nil), schema.UserMessage("current task")}
	large := strings.Repeat("large tool output\n", 7000)
	for i := 0; i < 20; i++ {
		preview, err := mw.modelToolOutput(context.Background(), large)
		if err != nil || len(preview) > maxInlineToolOutput || !strings.Contains(preview, "large-result") {
			t.Fatalf("result %d: %d bytes, err=%v", i, len(preview), err)
		}
		id := fmt.Sprintf("call-%d", i)
		messages = append(messages, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"big.txt"}`}}}}, &schema.Message{Role: schema.Tool, ToolName: "read_file", ToolCallID: id, Content: preview})
	}
	if outputs.stored != 20 || mw.outputBytes > maxInlineRunToolOutput {
		t.Fatalf("snapshots=%d inline=%d", outputs.stored, mw.outputBytes)
	}
	if _, err := finalizeCompaction(messages, schema.AssistantMessage("The current task and prior tool findings are summarized.", nil), 2); err != nil {
		t.Fatalf("20 oversized results prevented compaction: %v", err)
	}
}
func (s *capturedToolOutputs) ReadToolOutput(_ context.Context, x harness.Session, id string, offset int64, maxBytes int) (harness.ToolOutputChunk, error) {
	if x.ID != s.owner.ID || x.CWD != s.owner.CWD || id != "large-result" {
		return harness.ToolOutputChunk{}, harness.ErrPermissionDenied
	}
	if maxBytes <= 0 || maxBytes > 4096 {
		maxBytes = 4096
	}
	s.reads++
	end := min(int64(len(s.text)), offset+int64(maxBytes))
	return harness.ToolOutputChunk{Text: s.text[offset:end], Offset: offset, NextOffset: end, TotalBytes: int64(len(s.text)), EOF: end == int64(len(s.text))}, nil
}

func TestLargeWorkspaceReadStaysBoundedThroughNativeCompaction(t *testing.T) {
	cwd := t.TempDir()
	content := strings.Repeat("A large workspace file with durable details.\n", 3500)
	if err := os.WriteFile(filepath.Join(cwd, "big.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	outputs := &capturedToolOutputs{}
	model := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0, 1:
			return textStream("Earlier short answer"), nil
		case 2:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "read-big", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"big.txt"}`}}}}}), nil
		case 3:
			return textStream("Earlier work summarized"), nil
		case 4:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "read-snapshot", Type: "function", Function: schema.FunctionCall{Name: "read_tool_output", Arguments: `{"id":"large-result","offset":0,"max_bytes":1024}`}}}}}), nil
		default:
			return textStream("Finished after reading the file"), nil
		}
	}}
	engine := newTestEngine(t, Config{ChatModel: model, Model: "fixture", SessionStore: store, ToolFactory: tools.WorkspaceFactory, ToolOutputStore: outputs, PermissionMode: harness.PermissionModeOff, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 7, ContextTokens: 2_000_000, KeepRecentMessages: 1}})
	var largeEvent bool
	for i, prompt := range []string{"first", "second", "read the large file"} {
		req := request(string(rune('a' + i)))
		req.Session.CWD = cwd
		req.Input[0].Text = prompt
		_, err := engine.Run(context.Background(), req, func(_ context.Context, event harness.RunEvent) error {
			if event.Kind == "tool_end" && len(event.Content) > 0 && event.Content[0].Text == content {
				largeEvent = true
			}
			return nil
		}, nil)
		if err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
	if !largeEvent || outputs.text != content {
		t.Fatal("full tool result was not preserved outside the model context")
	}
	if model.calls < 6 || outputs.reads != 1 {
		t.Fatalf("native compaction or authorized snapshot read did not run: calls=%d reads=%d", model.calls, outputs.reads)
	}
	var sawPreview bool
	for _, call := range model.seen {
		for _, message := range call {
			if message.Role == schema.Tool && message.ToolCallID == "read-big" {
				if len(message.Content) > maxInlineToolOutput || strings.Contains(message.Content, content) || !strings.Contains(message.Content, "large-result") {
					t.Fatalf("unbounded or missing snapshot preview: %d bytes: %q", len(message.Content), message.Content)
				}
				sawPreview = true
			}
		}
	}
	if !sawPreview {
		t.Fatal("model never saw the bounded tool result")
	}
}

func TestEnhancedToolTextSnapshotPreservesNonTextParts(t *testing.T) {
	full := strings.Repeat("MCP detail ", 9000)
	image := schema.ToolOutputPart{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{}}
	input := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: full[:len(full)/2]}, image, {Type: schema.ToolPartTypeText, Text: full[len(full)/2:]}}}
	store := &capturedToolOutputs{}
	mw := &toolMiddleware{outputs: store, sink: &eventSink{request: harness.RunRequest{Session: harness.Session{ID: "owner", CWD: t.TempDir()}}}}
	got, err := mw.modelEnhancedToolOutput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) != 2 || got.Parts[1].Type != schema.ToolPartTypeImage || got.Parts[1].Image != image.Image || len(got.Parts[0].Text) > maxInlineToolOutput || !strings.Contains(got.Parts[0].Text, "large-result") {
		t.Fatalf("enhanced projection lost media or exceeded bound: %+v", got.Parts)
	}
	if input.Parts[0].Text+input.Parts[2].Text != full || store.text != input.Parts[0].Text+"\n\n"+input.Parts[2].Text {
		t.Fatal("full enhanced text was mutated or not snapshotted")
	}
}
