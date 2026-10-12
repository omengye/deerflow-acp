package eino

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestStoppedToolHistoryRepairsOnlyMissingHistoricalResults(t *testing.T) {
	call := func(ids ...string) *schema.Message {
		message := schema.AssistantMessage("calling tools", nil)
		for _, id := range ids {
			message.ToolCalls = append(message.ToolCalls, schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: "execute", Arguments: `{}`}})
		}
		return message
	}
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("old user request"), call("completed", "stopped"),
		schema.ToolMessage("confirmed output", "completed"),
		schema.UserMessage("current user request"), call("current-pending"),
	}}
	middleware := &stoppedToolHistory{}
	ctx := interaction.WithStoppedToolRecovery(context.Background())
	_, result, err := middleware.BeforeModelRewriteState(ctx, state, nil)
	if err != nil || result == state || len(result.Messages) != len(state.Messages)+1 {
		t.Fatalf("history repair failed: %+v %v", result, err)
	}
	if len(state.Messages) != 5 || result.Messages[2] != state.Messages[2] || result.Messages[4] != state.Messages[3] || result.Messages[5] != state.Messages[4] {
		t.Fatal("repair modified original state, recorded results, or the active turn")
	}
	repaired := result.Messages[3]
	if repaired.Role != schema.Tool || repaired.ToolCallID != "stopped" || repaired.ToolName != "execute" || !strings.Contains(repaired.Content, "unconfirmed") {
		t.Fatalf("repair claimed a result: %+v", repaired)
	}
	_, again, err := middleware.BeforeModelRewriteState(ctx, result, nil)
	if err != nil || again != result || !reflect.DeepEqual(again.Messages, result.Messages) {
		t.Fatal("history repair was not idempotent")
	}
	_, unmarked, err := middleware.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || unmarked != state {
		t.Fatal("history was changed without host-verified cancellation")
	}
}
