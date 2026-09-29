package runtime

import (
	"context"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBackgroundPermissionInheritedPolicyGrant(t *testing.T) {
	for _, policy := range []string{"plan", "read_only", "allow_always", "reject_always"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			f := newBackgroundPermissionFixture(t, func(host *backgroundHostFixture) {
				if policy == "plan" {
					host.spec.Parent.Mode = "plan"
				} else {
					host.spec.Parent.ApprovalMode = policy
				}
				if err := host.store.SaveConfig(ctx, host.spec.Parent); err != nil {
					t.Fatal(err)
				}
				var err error
				host.binding.ExecutionContract, err = host.spec.Contract()
				if err != nil {
					t.Fatal(err)
				}
				host.scope.Binding = host.binding
			})
			f.pending(t, "one")
			want, configured := configuredPermission(f.spec.Parent, f.requests[0])
			if !configured || want == "" {
				t.Fatal("fixture has no predetermined policy", want, configured)
			}
			decision, handled, err := f.attempt.ResolvePolicyPermission(ctx, f.requests[0], f.intents[0])
			if err != nil || !handled || decision != want {
				t.Fatal("inherited policy required human approval", decision, handled, err)
			}
			var approver, policySHA, state string
			if err = f.native.DB().QueryRow(`SELECT g.approved_by,p.policy_sha,g.state FROM harness_background_permission_grants g JOIN harness_background_policy_grants p ON p.grant_id=g.id WHERE g.task_id=?`, f.binding.TaskID).Scan(&approver, &policySHA, &state); err != nil || approver != "" || policySHA == "" || state != "ready" {
				t.Fatal("policy grant lacks an independent authority audit", approver, policySHA, state, err)
			}
			p := f.requests[0]
			event := harness.RunEvent{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "failed"}
			if decision == harness.AllowOnce {
				event.Kind, event.Status = "tool_execute", "in_progress"
			}
			if err = f.attempt.Publish(ctx, event); err != nil {
				t.Fatal(err)
			}
			if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_grants WHERE task_id=?`, f.binding.TaskID).Scan(&state); err != nil || state != "consumed" {
				t.Fatal("policy decision was not consumed with receipt", state, err)
			}
			if _, _, err = f.attempt.ResolvePolicyPermission(ctx, p, f.intents[0]); !errors.Is(err, harness.ErrExecutionConflict) {
				t.Fatal("policy grant could be reused", err)
			}
		})
	}
}

func TestBackgroundPermissionSuspendAfterApproval(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	ctx := context.Background()
	f.pending(t, "one")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	p := f.requests[0]
	for _, event := range []harness.RunEvent{
		{Kind: "tool_execute", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "in_progress"},
		{Kind: "tool_end", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "completed"},
	} {
		if err := f.attempt.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Checkpoint: []byte(`{"drain":"checkpoint after completed effect"}`)})
	if err != nil {
		t.Fatal(err)
	}
	task, err = f.tasks.ReleaseSuspension(ctx, &bt.ReleaseSuspensionRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Start(ctx, &bt.StartTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
		t.Fatalf("gracefully suspended checkpoint cannot start its next native attempt: %v", err)
	}
	f.scope.Attempt++
	if err = f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	next, err := f.permissions.OpenAttempt(ctx, f.scope, f.fence)
	if err != nil || len(next.Hooks().Targets) != 0 {
		t.Fatal("drain restored obsolete permission targets", next, err)
	}
	var state string
	if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_manifests WHERE task_id=?`, f.binding.TaskID).Scan(&state); err != nil || state != "closed" {
		t.Fatal("prior permission checkpoint was reused", state, err)
	}
}

func TestConfiguredPermissionReadOnlyBackgroundQueries(t *testing.T) {
	for _, session := range []harness.Session{{Mode: "plan"}, {ApprovalMode: harness.ApprovalReadOnly}} {
		for name, want := range map[string]harness.PermissionDecision{"task_status": harness.AllowOnce, "task_wait": harness.AllowOnce, "task_cancel": harness.RejectOnce, "background_agent": harness.RejectOnce} {
			got, handled := configuredPermission(session, harness.PermissionRequest{ToolName: name})
			if !handled || got != want {
				t.Fatalf("mode=%s approval=%s tool=%s got=%s handled=%t", session.Mode, session.ApprovalMode, name, got, handled)
			}
		}
	}
}

func suspendBackgroundPermissionFixture(t *testing.T, f *backgroundPermissionFixture) *bt.Task {
	t.Helper()
	ctx := context.Background()
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Checkpoint: []byte(`{"drain":"new native checkpoint"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestBackgroundPermissionDrainWithoutPriorApproval(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	for _, expected := range []int64{2, 3} {
		task := suspendBackgroundPermissionFixture(t, f)
		if _, err := f.tasks.ReleaseSuspension(context.Background(), &bt.ReleaseSuspensionRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
			t.Fatal(err)
		}
		f.open(t)
		if f.scope.Attempt != expected || len(f.attempt.Hooks().Targets) != 0 {
			t.Fatal("first or repeated drain restored incorrect authority", f.scope, f.attempt.Hooks().Targets)
		}
	}
}

func TestBackgroundPermissionDrainFrontierRejectsDrift(t *testing.T) {
	for _, released := range []bool{false, true} {
		for _, frontier := range []string{"checkpoint", "native", "event", "input", "policy"} {
			t.Run(map[bool]string{false: "before_release/", true: "before_start/"}[released]+frontier, func(t *testing.T) {
				ctx := context.Background()
				f := newBackgroundPermissionFixture(t)
				task := suspendBackgroundPermissionFixture(t, f)
				var err error
				if released {
					task, err = f.tasks.ReleaseSuspension(ctx, &bt.ReleaseSuspensionRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
					if err != nil {
						t.Fatal(err)
					}
				}
				switch frontier {
				case "checkpoint":
					_, err = f.native.DB().Exec(`UPDATE eino_checkpoints SET payload='advanced' WHERE id=?`, f.binding.TaskID+"/checkpoint")
				case "native":
					_, err = f.native.DB().Exec(`INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,'advanced','message','{}')`, f.binding.ChildSessionID)
				case "event":
					_, err = f.store.Append(ctx, harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "message_delta", Content: []harness.Content{{Type: "text", Text: "advanced"}}})
				case "input":
					_, err = f.native.DB().Exec(`UPDATE harness_inputs SET content='[]' WHERE run_id=?`, f.binding.TaskID)
				case "policy":
					_, err = f.native.DB().Exec(`UPDATE harness_session_configs SET version=version+1 WHERE session_id=?`, f.binding.ParentSessionID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if released {
					_, err = f.tasks.Start(ctx, &bt.StartTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
				} else {
					_, err = f.tasks.ReleaseSuspension(ctx, &bt.ReleaseSuspensionRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version})
				}
				if err == nil {
					t.Fatal("changed drain frontier resumed execution")
				}
			})
		}
	}
}

func TestBackgroundPermissionDrainRevokesUnusedGrantAtomically(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundPermissionFixture(t)
	f.pending(t, "one")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := f.resume(f.approval(harness.AllowOnce)); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	previous := f.attempt.Hooks().Targets["native/one"]
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.DB().Exec(`CREATE TRIGGER fail_drain BEFORE INSERT ON harness_background_drain_manifests BEGIN SELECT RAISE(ABORT,'drain fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Suspend(ctx, &bt.SuspendTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Checkpoint: []byte("next checkpoint")}); err == nil {
		t.Fatal("drain frontier failure was ignored")
	}
	f.assertState(t, "running", "")
	var state string
	if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_grants WHERE id=?`, previous.GrantID).Scan(&state); err != nil || state != "ready" {
		t.Fatal("rolled back drain revoked its grant", state, err)
	}
	if _, err = f.native.DB().Exec(`DROP TRIGGER fail_drain`); err != nil {
		t.Fatal(err)
	}
	task = suspendBackgroundPermissionFixture(t, f)
	if err = f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_grants WHERE id=?`, previous.GrantID).Scan(&state); err != nil || state != "revoked" {
		t.Fatal("drain retained old attempt authority", state, err)
	}
	if _, err = f.tasks.ReleaseSuspension(ctx, &bt.ReleaseSuspensionRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if _, err = f.attempt.ResolvePermission(ctx, f.requests[0], f.intents[0], previous.GrantID); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("unused old grant crossed drain boundary", err)
	}
	if len(f.attempt.Hooks().Targets) != 0 {
		t.Fatal("drain replayed old permission targets")
	}
}

func TestBackgroundPermissionPublicAlwaysIsRejected(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	f.pending(t, "one")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []harness.PermissionDecision{harness.AllowAlways, harness.RejectAlways} {
		request := f.approval(decision)
		if _, err := f.permissions.PrepareResume(context.Background(), f.actor(), f.binding, request); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("public approval implied unsupported decision caching", decision, err)
		}
		request.Decision = ""
		request.Evidence = []byte(`{"decisions":[{"intentId":"task-child/one","version":1,"decision":"` + string(decision) + `"}]}`)
		if _, err := f.permissions.PrepareResume(context.Background(), f.actor(), f.binding, request); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("evidence implied unsupported decision caching", decision, err)
		}
	}
}

func TestBackgroundPermissionPolicyGrantRollbackBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	f := newBackgroundPermissionFixture(t, func(host *backgroundHostFixture) {
		host.spec.Parent.ApprovalMode = harness.ApprovalAllowAlways
		if err := host.store.SaveConfig(ctx, host.spec.Parent); err != nil {
			t.Fatal(err)
		}
		var err error
		host.binding.ExecutionContract, err = host.spec.Contract()
		if err != nil {
			t.Fatal(err)
		}
		host.scope.Binding = host.binding
	})
	f.pending(t, "one")
	p, intent := f.requests[0], f.intents[0]
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_policy_authority BEFORE INSERT ON harness_background_policy_grants BEGIN SELECT RAISE(ABORT,'policy authority fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.attempt.ResolvePolicyPermission(ctx, p, intent); err == nil {
		t.Fatal("policy authority audit failure authorized a grant")
	}
	var count int
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatal("policy authority failure partly committed a grant", count, err)
	}
	event := harness.RunEvent{Kind: "tool_execute", ToolCallID: p.ToolCallID, ToolName: p.ToolName, Status: "in_progress"}
	if err := f.attempt.Publish(ctx, event); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("effect could pass after a policy grant transaction failed", err)
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_policy_authority`); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.ToolName = "different_tool"
	if _, _, err := f.attempt.ResolvePolicyPermission(ctx, wrong, intent); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatal("policy resolved mismatched intent", err)
	}
	if err := f.native.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatal("mismatched intent left a grant", count, err)
	}
	if decision, handled, err := f.attempt.ResolvePolicyPermission(ctx, p, intent); err != nil || !handled || decision != harness.AllowOnce {
		t.Fatal("valid policy could not prepare", decision, handled, err)
	}
	if _, err := f.native.DB().Exec(`CREATE TRIGGER fail_policy_dispatch BEFORE UPDATE ON harness_tool_receipts WHEN NEW.state='started' AND NEW.run_id='task-child' BEGIN SELECT RAISE(ABORT,'policy dispatch fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.attempt.Publish(ctx, event); err == nil {
		t.Fatal("failed receipt allowed policy dispatch")
	}
	var state string
	if err := f.native.DB().QueryRow(`SELECT state FROM harness_background_permission_grants`).Scan(&state); err != nil || state != "ready" {
		t.Fatal("failed receipt consumed policy grant", state, err)
	}
	if _, err := f.native.DB().Exec(`DROP TRIGGER fail_policy_dispatch`); err != nil {
		t.Fatal(err)
	}
	if err := f.attempt.Publish(ctx, event); err != nil {
		t.Fatal(err)
	}
}
