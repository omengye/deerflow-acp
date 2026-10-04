package eino

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestPreparedObserverWaitAndPermissionRoots(t *testing.T) {
	wait := &adk.InterruptCtx{ID: "native-wait", IsRootCause: true, Info: &interaction.TaskWaitInterrupt{Wait: interaction.TaskWait{TaskID: "descendant"}}}
	permission := &adk.InterruptCtx{ID: "native-permission", IsRootCause: true, Info: &permissionInterrupt{Intent: PermissionIntent{ID: "intent", Version: 1}}}
	wrapper := &adk.InterruptCtx{ID: "wrapper", Info: "native composite wrapper"}
	unknown := &adk.InterruptCtx{ID: "foreign", IsRootCause: true, Info: "unknown"}
	nilWait := &adk.InterruptCtx{ID: "nil-wait", IsRootCause: true, Info: (*interaction.TaskWaitInterrupt)(nil)}
	for _, test := range []struct {
		name                       string
		contexts                   []*adk.InterruptCtx
		wantWaits, wantPermissions int
		wantFailure                bool
	}{
		{"pure", []*adk.InterruptCtx{wrapper, wait}, 1, 0, false},
		{"mixed", []*adk.InterruptCtx{wrapper, wait, permission}, 1, 1, false},
		{"nil-wait", []*adk.InterruptCtx{nilWait}, 0, 0, true},
		{"unknown-alongside-wait", []*adk.InterruptCtx{wait, unknown}, 0, 0, true},
		{"no-root", []*adk.InterruptCtx{wrapper}, 0, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newTestEngine(t, Config{ChatModel: toolScript()})
			p, err := e.PrepareAttempt(context.Background(), request(test.name), "worker-v1", nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			waits, permissions := 0, 0
			event := &adk.AgentEvent{Action: &adk.AgentAction{Interrupted: &adk.InterruptInfo{InterruptContexts: test.contexts}}}
			agent := p.ObserveAgentWithTaskWaits(interruptObserverAgent{event: event}, func(_ context.Context, bindings []ExecutionInterruptBinding) error {
				permissions += len(bindings)
				return nil
			}, func(_ context.Context, bindings []interaction.TaskWaitBinding) error {
				waits += len(bindings)
				if len(bindings) != 1 || bindings[0].Wait.TaskID != "descendant" || bindings[0].NativeInterruptID != wait.ID {
					t.Errorf("wrong wait targets: %+v", bindings)
				}
				return nil
			}, nil)
			iter := agent.Run(context.Background(), nil)
			got, ok := iter.Next()
			if !ok || (got.Err != nil) != test.wantFailure || (got.Action == nil) != test.wantFailure {
				t.Fatalf("projection: %+v", got)
			}
			if _, ok = iter.Next(); ok {
				t.Fatal("unexpected second event")
			}
			if err = p.JoinAndClose(context.Background()); (err != nil) != test.wantFailure {
				t.Fatalf("join: %v", err)
			}
			if waits != test.wantWaits || permissions != test.wantPermissions {
				t.Fatalf("staged waits=%d permissions=%d", waits, permissions)
			}
		})
	}
}
