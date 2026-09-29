package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type backgroundToolsBackend struct {
	submit func(context.Context, harness.TaskActor, background.Binding, *adk.AgentInput, string) (harness.BackgroundTask, error)
	get    func(context.Context, harness.TaskActor, string) (harness.BackgroundTask, error)
	wait   func(context.Context, harness.TaskActor, string, int64) (harness.BackgroundTask, error)
	cancel func(context.Context, harness.TaskActor, string, string) (harness.BackgroundTask, error)
}

func (b *backgroundToolsBackend) SubmitNativeSubagent(ctx context.Context, actor harness.TaskActor, binding background.Binding, input *adk.AgentInput, description string) (harness.BackgroundTask, error) {
	if b.submit == nil {
		return harness.BackgroundTask{}, errors.New("unexpected submission")
	}
	return b.submit(ctx, actor, binding, input, description)
}
func (b *backgroundToolsBackend) Get(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	if b.get == nil {
		return harness.BackgroundTask{}, errors.New("unexpected get")
	}
	return b.get(ctx, actor, id)
}
func (b *backgroundToolsBackend) Wait(ctx context.Context, actor harness.TaskActor, id string, after int64) (harness.BackgroundTask, error) {
	if b.wait == nil {
		return harness.BackgroundTask{}, errors.New("unexpected wait")
	}
	return b.wait(ctx, actor, id, after)
}
func (b *backgroundToolsBackend) Cancel(ctx context.Context, actor harness.TaskActor, id, reason string) (harness.BackgroundTask, error) {
	if b.cancel == nil {
		return harness.BackgroundTask{}, errors.New("unexpected cancel")
	}
	return b.cancel(ctx, actor, id, reason)
}

// Obtain the unforgeable foreground actor through a real runtime admission.
// Neither tests nor model arguments can directly construct its context key.
func withBackgroundToolsRun(t *testing.T, check func(context.Context, harness.RunRequest, *hr.Service)) {
	t.Helper()
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := hr.NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	store.BudgetLedger, err = budget.New(native.DB(), budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	service := hr.NewService(store, nil, "test")
	service.Settings.EnableSubagents, service.Settings.DefaultSubagents = true, true
	x, err := service.NewSession(ctx, "tool-owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service.Engine = engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		check(ctx, req, service)
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	if _, err := service.Run(ctx, "tool-owner", x.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func backgroundToolsByName(t *testing.T, list []tool.BaseTool) map[string]tool.InvokableTool {
	t.Helper()
	result := make(map[string]tool.InvokableTool)
	for _, item := range list {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		result[info.Name] = item.(tool.InvokableTool)
	}
	return result
}

func invokeBackgroundTool(ctx context.Context, selected tool.BaseTool, name, callID, args string) (string, error) {
	node, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{Tools: []tool.BaseTool{selected}})
	if err != nil {
		return "", err
	}
	messages, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{{ID: callID, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}}))
	if err != nil {
		return "", err
	}
	if len(messages) != 1 {
		return "", errors.New("expected one native tool result")
	}
	return messages[0].Content, nil
}

func TestBackgroundToolsSubmitUsesRuntimeIdentityAndExactArguments(t *testing.T) {
	withBackgroundToolsRun(t, func(ctx context.Context, req harness.RunRequest, runtime *hr.Service) {
		const args = `{ "instruction": "child task", "description": "inspect", "childSessionId": "prior-child" }`
		state := json.RawMessage(`{"version":1,"skills":[],"sandboxPolicy":"disabled"}`)
		expectedState := string(state)
		calls := 0
		backend := &backgroundToolsBackend{submit: func(ctx context.Context, actor harness.TaskActor, b background.Binding, input *adk.AgentInput, description string) (harness.BackgroundTask, error) {
			calls++
			if actor.OwnerID != "tool-owner" || actor.SessionID != req.Session.ID || b.ParentSessionID != req.Session.ID || b.OriginRunID != req.RunID || b.OriginToolCallID != "native-call" || b.RootBudgetID != req.RootBudgetID || b.Workspace != req.Session.CWD || b.ConfigVersion != req.Session.ConfigVersion || b.ChildSessionID != "prior-child" || b.AgentVersion != backgroundAgentVersion || b.TaskID != "" {
				t.Fatalf("submission identity: actor=%+v binding=%+v req=%+v", actor, b, req)
			}
			spec, ok := ctx.Value(backgroundSpecKey{}).(hr.BackgroundExecutionSpec)
			if !ok || spec.Parent.ID != req.Session.ID || spec.OriginArguments != args || string(spec.Extension) != expectedState || spec.HostPolicy != "host-policy" || len(spec.Input) != 1 || spec.Input[0].Text != "child task" {
				t.Fatalf("invalid immutable spec: %+v", spec)
			}
			contract, err := spec.Contract()
			if err != nil || contract != b.ExecutionContract {
				t.Fatalf("contract=%s binding=%s err=%v", contract, b.ExecutionContract, err)
			}
			if description != "inspect" || input == nil || len(input.Messages) != 1 || input.Messages[0].Role != schema.User || input.Messages[0].Content != "child task" {
				t.Fatalf("child input=%+v description=%s", input, description)
			}
			return harness.BackgroundTask{ID: "accepted", SessionID: actor.SessionID, ChildSessionID: "prior-child", Status: "pending", Version: 1}, nil
		}}
		list, err := newBackgroundTools(ctx, req, state, "host-policy", backend, runtime.AuthorizeTaskAccess)
		if err != nil {
			t.Fatal(err)
		}
		// A caller mutating its JSON buffer cannot replace child resource policy.
		for i := range state {
			state[i] = 'x'
		}
		byName := backgroundToolsByName(t, list)
		output, err := invokeBackgroundTool(ctx, byName["background_agent"], "background_agent", "native-call", args)
		if err != nil || !strings.Contains(output, `"id":"accepted"`) || calls != 1 {
			t.Fatalf("output=%s calls=%d err=%v", output, calls, err)
		}
	})
}

func TestBackgroundToolsScopeAndInputFailuresNeverReachBackend(t *testing.T) {
	withBackgroundToolsRun(t, func(ctx context.Context, req harness.RunRequest, runtime *hr.Service) {
		backend := &backgroundToolsBackend{}
		state := json.RawMessage(`{"version":1,"skills":[],"sandboxPolicy":"disabled"}`)
		list, err := newBackgroundTools(ctx, req, state, "policy", backend, runtime.AuthorizeTaskAccess)
		if err != nil {
			t.Fatal(err)
		}
		byName := backgroundToolsByName(t, list)
		for _, args := range []string{`null`, `[]`, `{}`, `{"instruction":" "}`, `{"instruction":"ok","rootBudgetId":"foreign"}`, `{"instruction":"ok","sessionId":"foreign"}`, `{"instruction":"ok","originRunId":"foreign"}`, `{"instruction":"ok"} {}`, `{"instruction":"ok","childSessionId":"\u0000"}`} {
			if _, err := byName["background_agent"].InvokableRun(ctx, args); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("bad arguments %s reached backend: %v", args, err)
			}
		}
		if _, err := byName["background_agent"].InvokableRun(ctx, `{"instruction":"ok"}`); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("missing native call ID: %v", err)
		}
		if _, err := byName["task_status"].InvokableRun(context.Background(), `{"taskId":"id"}`); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("missing current actor: %v", err)
		}
		foreign := req
		foreign.Session.ID = "foreign-session"
		if _, err := newBackgroundTools(ctx, foreign, state, "policy", backend, runtime.AuthorizeTaskAccess); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("borrowed foreign actor: %v", err)
		}
		for name, args := range map[string]string{"task_status": `{"taskId":"id","sessionId":"foreign"}`, "task_cancel": `{"taskId":"id","reason":"model override"}`, "task_wait": `{"taskId":"id","afterVersion":0}`} {
			if _, err := byName[name].InvokableRun(ctx, args); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("invalid management args %s: %v", name, err)
			}
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := byName["task_status"].InvokableRun(cancelled, `{"taskId":"id"}`); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled operation reached backend: %v", err)
		}
	})
}

func TestBackgroundToolsAvailabilityMatchesResourcesAndPolicy(t *testing.T) {
	withBackgroundToolsRun(t, func(ctx context.Context, req harness.RunRequest, runtime *hr.Service) {
		for _, mode := range []string{"normal", "mcp", "plan", "readonly", "subagents_off"} {
			changed := req
			pinned := extensionState{Version: 1, SandboxPolicy: "disabled"}
			switch mode {
			case "mcp":
				pinned.MCPGeneration = "parent-connection-generation"
			case "plan":
				changed.Session.Mode = "plan"
			case "readonly":
				changed.Session.ApprovalMode = harness.ApprovalReadOnly
			case "subagents_off":
				changed.Session.Subagents = false
			}
			data, _ := json.Marshal(pinned)
			list, err := newBackgroundTools(ctx, changed, data, "policy", &backgroundToolsBackend{}, runtime.AuthorizeTaskAccess)
			if err != nil {
				t.Fatal(err)
			}
			names := backgroundToolsByName(t, list)
			if (names["background_agent"] != nil) != (mode == "normal") || names["task_status"] == nil || names["task_wait"] == nil || (names["task_cancel"] != nil) != (mode != "plan" && mode != "readonly") {
				t.Fatalf("mode=%s wrong tools: %+v", mode, names)
			}
		}
		if _, err := newBackgroundTools(ctx, req, []byte(`{"version":2}`), "policy", &backgroundToolsBackend{}, runtime.AuthorizeTaskAccess); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("unknown pinned resource version: %v", err)
		}
	})
}

func TestBackgroundToolsManagementRechecksAuthorityAndBoundsWait(t *testing.T) {
	withBackgroundToolsRun(t, func(ctx context.Context, req harness.RunRequest, runtime *hr.Service) {
		gets, cancels, waits := 0, 0, 0
		backend := &backgroundToolsBackend{
			get: func(_ context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
				gets++
				if actor.SessionID != req.Session.ID || id != "task" {
					t.Fatalf("get authority: %+v %s", actor, id)
				}
				return harness.BackgroundTask{ID: id, Status: "running", Version: 2}, nil
			},
			cancel: func(_ context.Context, actor harness.TaskActor, id, reason string) (harness.BackgroundTask, error) {
				cancels++
				if actor.SessionID != req.Session.ID || id != "task" || reason != "cancelled by parent agent" {
					t.Fatalf("cancel authority: %+v %s %s", actor, id, reason)
				}
				return harness.BackgroundTask{ID: id, Status: "running", Version: 3}, nil
			},
			wait: func(ctx context.Context, actor harness.TaskActor, id string, after int64) (harness.BackgroundTask, error) {
				waits++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > backgroundToolWaitLimit || actor.SessionID != req.Session.ID || id != "task" || after != 2 {
					t.Fatalf("unbounded or mis-scoped wait: %+v %s %d %v", actor, id, after, deadline)
				}
				return harness.BackgroundTask{}, context.DeadlineExceeded
			},
		}
		allowed := true
		authorize := func(ctx context.Context, actor harness.TaskActor) error {
			if !allowed {
				return harness.ErrNotAttached
			}
			return runtime.AuthorizeTaskAccess(ctx, actor)
		}
		list, err := newBackgroundTools(ctx, req, []byte(`{"version":1}`), "policy", backend, authorize)
		if err != nil {
			t.Fatal(err)
		}
		names := backgroundToolsByName(t, list)
		for name, args := range map[string]string{"task_status": `{"taskId":"task"}`, "task_cancel": `{"taskId":"task"}`, "task_wait": `{"taskId":"task","afterVersion":2}`} {
			out, err := names[name].InvokableRun(ctx, args)
			if err != nil || !strings.Contains(out, `"status":"running"`) || name == "task_wait" && !strings.Contains(out, `"timedOut":true`) {
				t.Fatalf("%s=%s: %v", name, out, err)
			}
		}
		if gets != 2 || waits != 1 || cancels != 1 {
			t.Fatalf("calls get=%d wait=%d cancel=%d", gets, waits, cancels)
		}
		allowed = false
		if _, err := names["task_cancel"].InvokableRun(ctx, `{"taskId":"task"}`); !errors.Is(err, harness.ErrNotAttached) || cancels != 1 {
			t.Fatalf("revoked attachment used cached authority: %v calls=%d", err, cancels)
		}
	})
}

func TestBackgroundToolsWaitPropagatesCallerCancellation(t *testing.T) {
	withBackgroundToolsRun(t, func(ctx context.Context, req harness.RunRequest, runtime *hr.Service) {
		waitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		backend := &backgroundToolsBackend{wait: func(ctx context.Context, _ harness.TaskActor, _ string, _ int64) (harness.BackgroundTask, error) {
			cancel()
			<-ctx.Done()
			return harness.BackgroundTask{}, ctx.Err()
		}}
		list, err := newBackgroundTools(waitCtx, req, []byte(`{"version":1}`), "policy", backend, runtime.AuthorizeTaskAccess)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backgroundToolsByName(t, list)["task_wait"].InvokableRun(waitCtx, `{"taskId":"task","afterVersion":1}`); !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation hidden: %v", err)
		}
	})
}

func TestBackgroundToolsAcceptedTaskSurvivesNotificationError(t *testing.T) {
	cause := errors.New("notification failed after task commit")
	_, err := backgroundToolResult(harness.BackgroundTask{ID: "accepted", Status: "pending", Version: 1}, false, cause)
	var receipt interface{ ToolResult() string }
	if !errors.Is(err, cause) || !errors.As(err, &receipt) || !strings.Contains(receipt.ToolResult(), `"id":"accepted"`) {
		t.Fatalf("accepted task ownership lost: %v", err)
	}
}

func TestBackgroundToolsTaskResultIsReadableWithoutChangingPublicBytes(t *testing.T) {
	for _, result := range []string{`{"answer":"child complete"}`, "child completed plain text"} {
		task := harness.BackgroundTask{ID: "task", Status: "completed", Result: []byte(result)}
		output, err := backgroundToolResult(task, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(output), &envelope); err != nil {
			t.Fatal(err)
		}
		want := result
		if !json.Valid([]byte(result)) {
			encoded, _ := json.Marshal(result)
			want = string(encoded)
		}
		if string(envelope.Result) != want || string(task.Result) != result {
			t.Fatalf("result=%s want=%s public=%s", envelope.Result, want, task.Result)
		}
	}
}
