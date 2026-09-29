package eino

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestNativeWriteTodosProducesPlanEvent(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "todo-1", Type: "function", Function: schema.FunctionCall{Name: "write_todos", Arguments: `{"todos":[{"content":"Inspect","activeForm":"Inspecting","status":"in_progress"},{"content":"Report","activeForm":"Reporting","status":"pending"}]}`}}}}}), nil
		}
		return textStream("done"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake})
	var events []harness.RunEvent
	result, err := e.Run(context.Background(), request("plan"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, nil)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("result=%+v err=%v events=%+v", result, err, events)
	}
	var plans []harness.RunEvent
	for _, event := range events {
		if event.Kind == "plan_update" {
			plans = append(plans, event)
		}
	}
	if len(plans) != 1 || len(plans[0].Plan) != 2 || plans[0].Plan[0].Content != "Inspect" || plans[0].Plan[0].Status != "in_progress" || plans[0].Plan[0].Priority != "medium" || plans[0].Plan[1].Status != "pending" {
		t.Fatalf("native plan events=%+v all=%+v", plans, events)
	}
}

func TestNativeChildTodosDoNotReplaceLeadPlan(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"inspect","description":"inspect"}`}}}}}), nil
		case 1:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "child-todo", Type: "function", Function: schema.FunctionCall{Name: "write_todos", Arguments: `{"todos":[{"content":"Child work","activeForm":"Working","status":"in_progress"}]}`}}}}}), nil
		case 2:
			return textStream("child done"), nil
		default:
			return textStream("parent done"), nil
		}
	}}
	e, err := New(context.Background(), Config{ChatModel: fake})
	if err != nil {
		t.Fatal(err)
	}
	var planEvents int
	result, err := e.Run(context.Background(), request("child-plan"), func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "plan_update" {
			planEvents++
		}
		return nil
	}, nil)
	if err != nil || result.StopReason != "end_turn" || planEvents != 0 {
		t.Fatalf("result=%+v err=%v lead plan updates=%d", result, err, planEvents)
	}
}
