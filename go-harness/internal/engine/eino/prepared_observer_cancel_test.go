package eino

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
)

type interruptObserverAgent struct {
	silentObserverAgent
	event *adk.AgentEvent
}

func (a interruptObserverAgent) Run(context.Context, *adk.AgentInput, ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, writer := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	writer.Send(a.event)
	writer.Close()
	return iter
}

func TestPreparedObserverNativeControlKeepsStrictPermissionBoundary(t *testing.T) {
	permission := &adk.InterruptCtx{ID: "native-permission", IsRootCause: true, Info: &permissionInterrupt{Intent: PermissionIntent{ID: "intent", Version: 1}}}
	system := &adk.InterruptCtx{ID: "opaque-native-system", IsRootCause: true, Info: struct{ System bool }{true}}
	malformed := &adk.InterruptCtx{ID: "malformed", IsRootCause: true, Info: &permissionInterrupt{}}
	for _, test := range []struct {
		name        string
		control     func() bool
		contexts    []*adk.InterruptCtx
		wantStages  int
		wantFailure bool
	}{
		{"unknown-without-control", nil, []*adk.InterruptCtx{system}, 0, true},
		{"unknown-before-control", func() bool { return false }, []*adk.InterruptCtx{system}, 0, true},
		{"native-system-during-control", func() bool { return true }, []*adk.InterruptCtx{system}, 0, false},
		{"permission-during-control", func() bool { return true }, []*adk.InterruptCtx{permission}, 1, false},
		{"mixed-during-control", func() bool { return true }, []*adk.InterruptCtx{permission, system}, 1, false},
		{"mixed-without-control", nil, []*adk.InterruptCtx{permission, system}, 0, true},
		{"invalid-permission-during-control", func() bool { return true }, []*adk.InterruptCtx{system, malformed}, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newTestEngine(t, Config{ChatModel: toolScript()})
			p, err := e.PrepareAttempt(context.Background(), request(test.name), "worker-v1", nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			event := &adk.AgentEvent{Action: &adk.AgentAction{Interrupted: &adk.InterruptInfo{InterruptContexts: test.contexts}}}
			stages := 0
			agent := p.ObserveAgentWithNativeCancel(interruptObserverAgent{event: event}, func(_ context.Context, bindings []ExecutionInterruptBinding) error {
				stages++
				if len(bindings) != 1 || bindings[0].IntentID != "intent" || bindings[0].NativeInterruptID != "native-permission" {
					t.Errorf("changed permission bindings: %+v", bindings)
				}
				return nil
			}, test.control)
			iter := agent.Run(context.Background(), nil)
			got, ok := iter.Next()
			if !ok || (got.Err != nil) != test.wantFailure || (got.Action == nil) != test.wantFailure {
				t.Fatalf("interrupt projection=%+v wantFailure=%v", got, test.wantFailure)
			}
			if _, ok = iter.Next(); ok {
				t.Fatal("unexpected second event")
			}
			if err = p.JoinAndClose(context.Background()); (err != nil) != test.wantFailure {
				t.Fatalf("joined outcome=%v wantFailure=%v", err, test.wantFailure)
			}
			if stages != test.wantStages {
				t.Fatalf("permission stages=%d want=%d", stages, test.wantStages)
			}
		})
	}
}

func TestPreparedObserverNativeControlDoesNotSwallowEventFailure(t *testing.T) {
	want := errors.New("native event failed after drain request")
	e := newTestEngine(t, Config{ChatModel: toolScript()})
	p, err := e.PrepareAttempt(context.Background(), request("drain-event-error"), "worker-v1", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := &adk.AgentEvent{Err: want, Action: &adk.AgentAction{Interrupted: &adk.InterruptInfo{InterruptContexts: []*adk.InterruptCtx{{ID: "system", IsRootCause: true}}}}}
	iter := p.ObserveAgentWithNativeCancel(interruptObserverAgent{event: event}, nil, func() bool { return true }).Run(context.Background(), nil)
	got, ok := iter.Next()
	if !ok || !errors.Is(got.Err, want) || got.Action != event.Action {
		t.Fatalf("native event lost: %+v", got)
	}
	if _, ok = iter.Next(); ok {
		t.Fatal("unexpected second event")
	}
	if err := p.JoinAndClose(context.Background()); err != nil {
		t.Fatalf("native error contaminated resource cleanup: %v", err)
	}
}
