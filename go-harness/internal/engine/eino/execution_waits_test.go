package eino

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestForegroundInterruptsSeparateWaitsAndPermissions(t *testing.T) {
	wait := interaction.TaskWait{TaskID: "child", AfterVersion: 1, CallID: "wait", SessionID: "session", RunID: "run", RootBudgetID: "root", RoleID: "role"}
	permission := &permissionInterrupt{Intent: PermissionIntent{ID: "intent", Version: 1}, CallID: "write"}
	contexts := []*adk.InterruptCtx{
		{ID: "wrapper", Info: "composite", IsRootCause: false},
		{ID: "wait-target", Info: &interaction.TaskWaitInterrupt{Wait: wait}, IsRootCause: true},
		{ID: "permission-target", Info: permission, IsRootCause: true},
	}
	bindings, waits, err := collectExecutionInterrupts(contexts, true)
	if err != nil || len(bindings) != 1 || len(waits) != 1 || waits[0].Wait != wait || bindings[0].IntentID != "intent" {
		t.Fatalf("mixed bindings: %+v %+v %v", bindings, waits, err)
	}
	for _, kind := range []string{"unsupported", "nil_wait", "unknown_root", "duplicate_target", "duplicate_call"} {
		t.Run(kind, func(t *testing.T) {
			copyOf := append([]*adk.InterruptCtx(nil), contexts...)
			allow := true
			switch kind {
			case "unsupported":
				allow = false
			case "nil_wait":
				copyOf[1] = &adk.InterruptCtx{ID: "wait-target", Info: (*interaction.TaskWaitInterrupt)(nil), IsRootCause: true}
			case "unknown_root":
				copyOf = append(copyOf, &adk.InterruptCtx{ID: "unknown", Info: "not a supported interrupt", IsRootCause: true})
			case "duplicate_target":
				copyOf = append(copyOf, &adk.InterruptCtx{ID: "wait-target", Info: permission, IsRootCause: true})
			case "duplicate_call":
				copyOf = append(copyOf, &adk.InterruptCtx{ID: "second-wait-target", Info: &interaction.TaskWaitInterrupt{Wait: wait}, IsRootCause: true})
			}
			if _, _, err := collectExecutionInterrupts(copyOf, allow); err == nil {
				t.Fatal("invalid root context accepted")
			}
		})
	}
}

func TestForegroundWaitResumeUsesOnlyTrustedReadyTargets(t *testing.T) {
	wait := interaction.TaskWaitBinding{Wait: interaction.TaskWait{TaskID: "child", AfterVersion: 1, CallID: "wait", SessionID: "session", RunID: "run", RootBudgetID: "root"}, NativeInterruptID: "wait-target"}
	permission := ExecutionInterruptBinding{IntentID: "intent", IntentVersion: 1, NativeInterruptID: "permission-target"}
	grant := PermissionResume{IntentID: "intent", IntentVersion: 1, GrantID: "grant"}
	for _, kind := range []string{"pure_wait", "mixed_pending_wait", "mixed_ready_wait", "no_ready_wait", "foreign_wait", "changed_wait", "missing_permission", "old_capability"} {
		t.Run(kind, func(t *testing.T) {
			saved := &checkpointEnvelope{TaskWaitsEnabled: true, TaskWaits: []interaction.TaskWaitBinding{wait}}
			hooks := &ExecutionHooks{Broker: &fixtureInteractionBroker{}, StageCheckpoint: func(context.Context, StagedExecutionCheckpoint) error { return nil }, TaskWaitsEnabled: true, WaitTargets: map[string]interaction.TaskWait{wait.NativeInterruptID: wait.Wait}}
			wantTargets, wantError := 1, false
			switch kind {
			case "mixed_pending_wait", "mixed_ready_wait", "missing_permission":
				saved.Interrupts = []ExecutionInterruptBinding{permission}
				hooks.Targets = map[string]PermissionResume{permission.NativeInterruptID: grant}
				wantTargets = 2
				if kind == "mixed_pending_wait" {
					hooks.WaitTargets = nil
					wantTargets = 1
				}
				if kind == "missing_permission" {
					hooks.Targets = nil
					wantError = true
				}
			case "no_ready_wait":
				hooks.WaitTargets = nil
				wantError = true
			case "foreign_wait":
				hooks.WaitTargets["forged"] = wait.Wait
				wantError = true
			case "changed_wait":
				changed := wait.Wait
				changed.RoleID = "other"
				hooks.WaitTargets[wait.NativeInterruptID] = changed
				wantError = true
			case "old_capability":
				saved.TaskWaitsEnabled = false
				wantError = true
			}
			params, err := resumeParams(hooks, saved)
			if wantError {
				if err == nil {
					t.Fatal("unsafe wait resume accepted")
				}
			} else if err != nil || params == nil || len(params.Targets) != wantTargets {
				t.Fatalf("resume params=%+v err=%v", params, err)
			}
		})
	}
}
