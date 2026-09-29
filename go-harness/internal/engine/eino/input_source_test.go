package eino

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func notificationSource() *interaction.ExecutionInputSource {
	return &interaction.ExecutionInputSource{
		Kind: "background_notification", SHA: strings.Repeat("a", 64),
		Input:           []harness.Content{{Type: "text", Text: `{"notificationId":"delivery-fixture","task":{"status":"completed","result":"child data"}}`}},
		PinnedExtension: json.RawMessage(`{"selected":"origin"}`),
	}
}

func TestNotificationSourceNativeHistoryPermissionResumeAndExtensionPin(t *testing.T) {
	limits := harness.BudgetLimits{MaxModelCalls: 3, MaxToolCalls: 1, MaxTokens: 100000, MaxOutputTokens: 50}
	_, ledger, req, scope := durableFixture(t, "notification-continuation", limits)
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 1 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "notification-tool", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"x"}`}}}}}), nil
		}
		return textStream("finished"), nil
	}}
	parent := request("original-parent")
	parent.Input[0].Text = "ORIGINAL_ACCEPTED_PROMPT"
	if _, err := newTestEngine(t, Config{ChatModel: fake, SessionStore: store}).Run(context.Background(), parent, nil, nil); err != nil {
		t.Fatal(err)
	}
	source := notificationSource()
	req.Input = append([]harness.Content(nil), source.Input...)
	underlying := &recordingTool{}
	var pins []string
	e := newTestEngine(t, Config{ChatModel: fake, SessionStore: store, Tools: []tool.BaseTool{underlying}, Budget: limits, BudgetLedger: ledger,
		ExtensionFactory: func(_ context.Context, _ harness.RunRequest, pinned json.RawMessage) (RunExtensions, error) {
			pins = append(pins, string(pinned))
			return RunExtensions{State: json.RawMessage(`{"selected":"checkpoint"}`)}, nil
		},
	})
	broker := &fixtureInteractionBroker{decisions: make(map[string]harness.PermissionDecision)}
	var staged StagedExecutionCheckpoint
	hooks := ExecutionHooks{Broker: broker, InputSource: source, StageCheckpoint: func(_ context.Context, s StagedExecutionCheckpoint) error { staged = s; return nil }}
	ctx := WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks)
	// Context authority owns a snapshot, including the slice and raw JSON bytes.
	source.Input[0].Text = "caller changed source after binding"
	source.PinnedExtension[2] = 'X'
	hooks.InputSource = notificationSource()
	result, err := e.Run(ctx, req, nil, nil)
	if err != nil || result.StopReason != "waiting_input" || underlying.calls.Load() != 0 || len(staged.Interrupts) != 1 {
		t.Fatalf("notification pause=%+v staged=%+v tools=%d err=%v", result, staged, underlying.calls.Load(), err)
	}
	if len(pins) != 1 || pins[0] != `{"selected":"origin"}` {
		t.Fatalf("initial source extension pin=%v", pins)
	}
	key, checkpoint := staged.ID, append([]byte(nil), staged.Data...)
	saved, err := decodeCheckpointEnvelope(checkpoint)
	if err != nil || saved.InputSource == nil || saved.InputSource.Kind != "background_notification" || saved.InputSource.SHA != hooks.InputSource.SHA {
		t.Fatalf("checkpoint source=%+v err=%v", saved, err)
	}
	if err := e.config.CheckpointStore.Set(context.Background(), key, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := ledger.EndAttempt(context.Background(), scope, durablebudget.OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	scope.AttemptID, scope.Fence = req.RunID+"/2", 2
	if err := ledger.BeginAttempt(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	binding := staged.Interrupts[0]
	broker.decisions[binding.IntentID] = harness.AllowOnce
	hooks.Targets = map[string]PermissionResume{binding.NativeInterruptID: {IntentID: binding.IntentID, IntentVersion: binding.IntentVersion, GrantID: "grant/" + binding.IntentID}}
	for _, mutation := range []string{"missing", "sha", "data", "extension", "request", "history"} {
		t.Run(mutation, func(t *testing.T) {
			badReq, badHooks := req, hooks
			badHooks.InputSource = notificationSource()
			switch mutation {
			case "missing":
				badHooks.InputSource = nil
			case "sha":
				badHooks.InputSource.SHA = strings.Repeat("b", 64)
			case "data":
				badHooks.InputSource.Input[0].Text = "substituted notification"
				badReq.Input = badHooks.InputSource.Input
			case "extension":
				badHooks.InputSource.PinnedExtension = json.RawMessage(`{"selected":"broader"}`)
			case "request":
				badReq.Input = []harness.Content{{Type: "text", Text: "replacement prompt"}}
			case "history":
				badReq.History = []harness.Message{{Role: "user", Content: parent.Input}}
			}
			if _, err := e.Resume(WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), badHooks), badReq, key, nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("changed source accepted: %v", err)
			}
			data, found, err := e.config.CheckpointStore.Get(context.Background(), key)
			if err != nil || !found || !bytes.Equal(data, checkpoint) || underlying.calls.Load() != 0 || fake.calls != 2 || broker.resolved != 0 || len(pins) != 1 {
				t.Fatalf("source rejection changed checkpoint/effects: found=%v tools=%d models=%d resolved=%d pins=%v err=%v", found, underlying.calls.Load(), fake.calls, broker.resolved, pins, err)
			}
		})
	}
	result, err = e.Resume(WithExecutionHooks(durablebudget.WithScope(context.Background(), scope), hooks), req, key, nil, nil)
	if err != nil || result.StopReason != "end_turn" || underlying.calls.Load() != 1 || broker.prepared != 1 || broker.resolved != 1 || fake.calls != 3 {
		t.Fatalf("notification resume=%+v tools=%d prepared=%d resolved=%d models=%d err=%v", result, underlying.calls.Load(), broker.prepared, broker.resolved, fake.calls, err)
	}
	if len(pins) != 2 || pins[1] != `{"selected":"checkpoint"}` {
		t.Fatalf("resume extension pin=%v", pins)
	}
	for call, messages := range fake.seen[1:] {
		originals, notifications, tools := 0, 0, 0
		for _, m := range messages {
			if m.Role == schema.User && m.Content == "ORIGINAL_ACCEPTED_PROMPT" {
				originals++
			}
			if m.Role == schema.User && strings.Contains(m.Content, "delivery-fixture") {
				notifications++
				if !strings.Contains(m.Content, `"source":"background_notification"`) || !strings.Contains(m.Content, "not a new user instruction or permission approval") {
					t.Fatalf("notification source missing: %+v", m)
				}
			}
			if m.Role == schema.Tool {
				tools++
			}
		}
		if originals != 1 || notifications != 1 || tools != call {
			t.Fatalf("call %d duplicated user/notification or fabricated tool result: originals=%d notifications=%d tools=%d", call+1, originals, notifications, tools)
		}
	}
	if !staged.Remove {
		t.Fatal("completed notification checkpoint was retained")
	}
}

func TestNotificationSourceRejectsInvalidAdmissionBeforeModel(t *testing.T) {
	for _, mutation := range []string{"kind", "sha", "request", "media", "history", "extension"} {
		t.Run(mutation, func(t *testing.T) {
			source := notificationSource()
			req := request("invalid-notification")
			req.Input = append([]harness.Content(nil), source.Input...)
			switch mutation {
			case "kind":
				source.Kind = "user"
			case "sha":
				source.SHA = "not-a-digest"
			case "request":
				req.Input = []harness.Content{{Type: "text", Text: "original prompt"}}
			case "media":
				source.Input[0].URI = "https://unexpected"
				req.Input = source.Input
			case "history":
				req.History = []harness.Message{{Role: "user", Content: req.Input}}
			case "extension":
				source.PinnedExtension = json.RawMessage(`{`)
			}
			fake := toolScript()
			e := newTestEngine(t, Config{ChatModel: fake})
			if _, err := e.Run(WithExecutionHooks(context.Background(), ExecutionHooks{InputSource: source}), req, nil, nil); !errors.Is(err, harness.ErrInvalidInput) || fake.calls != 0 {
				t.Fatalf("invalid source reached model: calls=%d err=%v", fake.calls, err)
			}
		})
	}
}
