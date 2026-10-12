package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func progressPair(id, name, args, result string) []*schema.Message {
	return []*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}}), schema.ToolMessage(result, id, schema.WithToolName(name))}
}

func TestProgressGuardCyclesAndExemptions(t *testing.T) {
	for _, name := range []string{"read_file", "task_wait", "task_status", "read_attachment", "bash", "write_file"} {
		t.Run(name, func(t *testing.T) {
			var state progressState
			messages := []*schema.Message{schema.UserMessage("work")}
			for i := 0; i < 5; i++ {
				messages = append(messages, progressPair(fmt.Sprint(i), name, `{"path":"a"}`, "same")...)
				advanceProgress(&state, messages)
			}
			if stopped := repeatedCycles(state.Recent) >= 5; stopped != (name == "read_file") {
				t.Fatalf("%s %+v", name, state)
			}
			previous := len(state.Recent)
			advanceProgress(&state, messages)
			if len(state.Recent) != previous {
				t.Fatal("double-counted history")
			}
		})
	}
	var state progressState
	for i := 0; i < 5; i++ {
		// Reconstructing middleware after compression preserves the checkpoint
		// counters even when only the latest pair remains in active messages.
		messages := append([]*schema.Message{schema.UserMessage("work")}, progressPair(fmt.Sprint(i), "bash", `{"command":"bad"}`, "Tool failed: config")...)
		advanceProgress(&state, messages)
		raw, _ := json.Marshal(state)
		state = progressState{}
		json.Unmarshal(raw, &state)
	}
	if repeatedCycles(state.Recent) != 5 || state.Warning == "" {
		t.Fatalf("lost checkpoint: %+v", state)
	}
	advanceProgress(&state, progressPair("mutation", "bash", `{"command":"fix"}`, "OK"))
	if len(state.Recent) != 0 {
		t.Fatal("successful mutation must reset evidence")
	}
	if repeatedCycles([]string{"a", "b", "a", "b", "a", "b", "a", "b", "a", "b"}) != 5 {
		t.Fatal("alternating cycle not detected")
	}
}

func TestProgressGuardRealEngineStopsIncomplete(t *testing.T) {
	reader, err := utils.InferTool("read_file", "Read", func(context.Context, struct {
		Path string `json:"path"`
	}) (string, error) {
		return "unchanged", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &scriptedModel{stream: func(_ context.Context, n int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray(progressPair(fmt.Sprint(n), "read_file", `{"path":"a"}`, "unused")[:1]), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{reader}, MaxIterations: 10, PermissionMode: harness.PermissionModeOff})
	_, err = e.Run(context.Background(), request("progress"), func(context.Context, harness.RunEvent) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "without completing") {
		t.Fatalf("err=%v calls=%d", err, fake.calls)
	}
	if fake.calls != 5 {
		t.Fatalf("calls=%d", fake.calls)
	}
	warned := false
	for _, m := range fake.seen[3] {
		warned = warned || strings.Contains(m.Content, "Progress warning:")
	}
	if !warned {
		t.Fatal("no change-strategy warning before stop")
	}
}

func TestProgressGuardSurvivesPermissionCheckpoint(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 10, MaxToolCalls: 10, MaxTokens: 100000, MaxOutputTokens: 256}
	_, ledger, req, scope := durableFixture(t, "progress-resume", limits)
	reader, err := utils.InferTool("read_file", "read", func(context.Context, struct {
		Path string `json:"path"`
	}) (string, error) {
		return "same", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(_ context.Context, n int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray(progressPair(fmt.Sprint(n), "read_file", `{"path":"a"}`, "")[:1]), nil
	}}}
	e := newTestEngine(t, Config{ChatModel: fake, Tools: []tool.BaseTool{reader}, Budget: limits, BudgetLedger: ledger})
	broker := &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}
	var staged StagedExecutionCheckpoint
	hooks := ExecutionHooks{Broker: broker, StageCheckpoint: func(_ context.Context, s StagedExecutionCheckpoint) error { staged = s; return nil }}
	ctx := WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
	result, err := e.Run(ctx, req, nil, nil)
	for i := 0; i < 5; i++ {
		if err != nil || result.StopReason != "waiting_input" || len(staged.Interrupts) != 1 {
			t.Fatalf("pause %d: %+v %v", i, result, err)
		}
		if err = e.config.CheckpointStore.Set(context.Background(), staged.ID, staged.Data); err != nil {
			t.Fatal(err)
		}
		if err = ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeWaitingInput); err != nil {
			t.Fatal(err)
		}
		scope.Fence++
		scope.AttemptID = fmt.Sprintf("%s/%d", req.RunID, scope.Fence)
		if err = ledger.BeginAttempt(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		binding := staged.Interrupts[0]
		broker.decisions[binding.IntentID] = harness.AllowOnce
		hooks.Targets = map[string]PermissionResume{binding.NativeInterruptID: {IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}}
		ctx = WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
		result, err = e.Resume(ctx, req, staged.ID, nil, nil)
	}
	if err == nil || !strings.Contains(err.Error(), "without completing") || fake.calls != 5 {
		t.Fatalf("progress reset on resume: calls=%d result=%+v err=%v", fake.calls, result, err)
	}
}
