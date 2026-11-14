package eino

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestProgressGuardStopsIdenticalFailureLoops(t *testing.T) {
	var state progressState
	messages := []*schema.Message{schema.UserMessage("work")}
	stoppedAt := 0
	for i := 0; i < 12 && stoppedAt == 0; i++ {
		messages = append(messages, progressPair(fmt.Sprint(i), "bash", `{"script":"make"}`, "Tool failed: exit status 2\nTool output: same error")...)
		advanceProgress(&state, messages)
		if shouldStopWithoutProgress(state) {
			stoppedAt = i + 1
		}
		state.Warned = state.Warning != ""
	}
	if stoppedAt != failedNoProgressStopCycles {
		t.Fatalf("identical failures stopped at %d, want %d", stoppedAt, failedNoProgressStopCycles)
	}
	if !strings.Contains(state.Warning, "will stop this run") {
		t.Fatalf("failure loop warning does not announce the stop: %q", state.Warning)
	}
}

func TestProgressGuardIgnoresRandomSnapshotIDs(t *testing.T) {
	var state progressState
	messages := []*schema.Message{schema.UserMessage("work")}
	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("[Tool output: 9000 UTF-8 bytes; session snapshot %q. Read with read_tool_output using id=%q and offset=0.]\nsame preview", fmt.Sprint("id", i), fmt.Sprint("id", i))
		messages = append(messages, progressPair(fmt.Sprint(i), "read_file", `{"path":"big.txt"}`, content)...)
		advanceProgress(&state, messages)
	}
	if repeatedCycles(state.Recent) < noProgressStopCycles {
		t.Fatalf("snapshot IDs hid a repeated observation: %+v", state)
	}
}

func TestFailedToolObservationRequiresHostPrefix(t *testing.T) {
	for content, want := range map[string]bool{
		"Tool failed: boom":                                   true,
		"Tool permission denied; ask the user":                true,
		"[Tool output: 9000 UTF-8 bytes]\nTool failed: boom":  true,
		"log line mentioning Tool failed: in a file":          false,
		"[Tool output: 9000 UTF-8 bytes]\nordinary file text": false,
	} {
		if got := failedToolObservation(content); got != want {
			t.Fatalf("%q: got %v", content, got)
		}
	}
}

func TestSmallToolResultsStayInlineAfterAllowanceIsSpent(t *testing.T) {
	outputs := &capturedToolOutputs{}
	mw := &toolMiddleware{outputs: outputs, sink: &eventSink{request: harness.RunRequest{Session: harness.Session{ID: "owner", CWD: t.TempDir()}}}}
	large := strings.Repeat("x", 20<<10)
	for i := 0; i < 4; i++ {
		if _, err := mw.modelToolOutput(context.Background(), large); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mw.modelToolOutput(context.Background(), "Tool failed: exit status 1")
	if err != nil || got != "Tool failed: exit status 1" {
		t.Fatalf("short failure hidden behind a snapshot: %q err=%v", got, err)
	}
	medium := strings.Repeat("y", 5<<10)
	got, err = mw.modelToolOutput(context.Background(), medium)
	if err != nil || !strings.HasPrefix(got, "[Tool output:") || !strings.Contains(got, "yyyy") {
		t.Fatalf("exhausted allowance lost the preview: %q err=%v", got, err)
	}
	// The allowance is replenished before the next model request.
	if _, _, err := mw.BeforeModelRewriteState(context.Background(), &adk.ChatModelAgentState{}, nil); err != nil {
		t.Fatal(err)
	}
	got, err = mw.modelToolOutput(context.Background(), medium)
	if err != nil || got != medium {
		t.Fatalf("allowance was not replenished per model call: %d bytes err=%v", len(got), err)
	}
}

func TestOversizedAndInvalidToolOutputIsBoundedNotFatal(t *testing.T) {
	outputs := &capturedToolOutputs{}
	mw := &toolMiddleware{outputs: outputs, sink: &eventSink{request: harness.RunRequest{Session: harness.Session{ID: "owner", CWD: t.TempDir()}}}}
	huge := strings.Repeat("z", maxToolOutputSnapshot+1000) + "\xff"
	got, err := mw.modelToolOutput(context.Background(), huge)
	if err != nil || len(outputs.text) > maxToolOutputSnapshot || !strings.Contains(got, "were dropped") {
		t.Fatalf("stored=%d err=%v preview=%q", len(outputs.text), err, got[:min(len(got), 300)])
	}
	got, err = mw.modelToolOutput(context.Background(), "bad \xff byte")
	if err != nil || !strings.Contains(got, "�") {
		t.Fatalf("invalid UTF-8 not sanitized: %q err=%v", got, err)
	}
}

func TestCompactionShrinksOversizedRecentWindow(t *testing.T) {
	messages := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old"), schema.AssistantMessage("old answer", nil), schema.UserMessage("current task")}
	big := strings.Repeat("w", 30<<10)
	for i := 0; i < 4; i++ {
		id := fmt.Sprint("call-", i)
		messages = append(messages, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "bash", Arguments: `{}`}}}}, &schema.Message{Role: schema.Tool, ToolName: "bash", ToolCallID: id, Content: big})
	}
	result, err := finalizeCompaction(messages, schema.AssistantMessage("summary", nil), 8)
	if err != nil {
		t.Fatalf("oversized recent window failed compaction: %v", err)
	}
	foundActive := false
	for i, m := range result {
		if m.Content == "current task" {
			foundActive = true
		}
		if m.Role == schema.Tool && (i == 0 || result[i-1].Role != schema.Assistant && result[i-1].Role != schema.Tool) {
			t.Fatalf("orphaned tool result at %d", i)
		}
	}
	if !foundActive || len(result) >= len(messages) {
		t.Fatalf("active request lost or nothing removed: %d messages", len(result))
	}
}

func TestProviderContextOverflowClassification(t *testing.T) {
	claude := &anthropic.Error{StatusCode: 400}
	if err := claude.UnmarshalJSON([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 205000 tokens > 200000 maximum"}}`)); err != nil {
		t.Fatal(err)
	}
	claude.StatusCode = 400
	for _, err := range []error{
		claude,
		&openaiapi.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded"},
		&openaiapi.APIError{HTTPStatusCode: 400, Message: "This model's maximum context length is 128000 tokens"},
	} {
		if !errors.Is(classifyModelContextError(fmt.Errorf("provider: %w", err)), harness.ErrContextLimit) {
			t.Fatalf("not classified as context limit: %T", err)
		}
	}
	if errors.Is(classifyModelContextError(&openaiapi.APIError{HTTPStatusCode: 400, Message: "invalid tool schema"}), harness.ErrContextLimit) {
		t.Fatal("ordinary bad request classified as context limit")
	}
}

func TestTransientProviderFailuresAreRetriedBeforeStreaming(t *testing.T) {
	for _, cause := range []error{
		&openaiapi.APIError{HTTPStatusCode: http.StatusServiceUnavailable},
		&anthropic.Error{StatusCode: 529},
	} {
		transient, ok := retryableRejection(rejectedRateLimitRequest(fmt.Errorf("provider: %w", cause)))
		if !ok || !transient {
			t.Fatalf("%T not retryable as transient", cause)
		}
	}
	for _, cause := range []error{context.Canceled, &openaiapi.APIError{HTTPStatusCode: 400}} {
		if _, ok := retryableRejection(rejectedRateLimitRequest(cause)); ok {
			t.Fatalf("%v must not be retried", cause)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
			if call < 2 {
				return nil, &openaiapi.APIError{HTTPStatusCode: http.StatusBadGateway}
			}
			return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("recovered", nil)}), nil
		}}
		m := rateLimitTracked(context.Background(), fake, harness.BudgetLimits{MaxModelCalls: 8, MaxOutputTokens: 64})
		answer, err := callRateLimitTracked(context.Background(), m, "stream")
		m.io.closeAndWait()
		if err != nil || answer != "recovered" || fake.calls != 3 || time.Since(start) != 6*time.Second {
			t.Fatalf("answer=%q err=%v calls=%d elapsed=%v", answer, err, fake.calls, time.Since(start))
		}
	})
}
