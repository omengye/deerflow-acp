package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func rolePointer[T any](v T) *T { return &v }

func TestConfiguredRoleUsesOwnModelAndRestrictedTools(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tools    []string
		deny     []string
		wantRead bool
	}{
		{"selected", []string{"read_workspace"}, nil, true},
		{"empty", []string{}, nil, false},
		{"host-denied", []string{"read_workspace"}, []string{"read_workspace"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var childCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model    string                           `json:"model"`
					Messages []struct{ Role, Content string } `json:"messages"`
					Tools    []struct {
						Function struct{ Name string } `json:"function"`
					} `json:"tools"`
					MaxTokens           int     `json:"max_tokens"`
					MaxCompletionTokens int     `json:"max_completion_tokens"`
					ReasoningEffort     string  `json:"reasoning_effort"`
					Temperature         float64 `json:"temperature"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture request", 400)
					return
				}
				if body.Model != "role-wire-model" || r.Header.Get("Authorization") != "Bearer child-fixture-key" {
					t.Error("role used incorrect model route or credentials")
				}
				var names []string
				for _, entry := range body.Tools {
					names = append(names, entry.Function.Name)
				}
				if slices.Contains(names, "read_workspace") != tc.wantRead || slices.Contains(names, "write_todos") || slices.Contains(names, "task") || slices.Contains(names, "background_agent") {
					t.Errorf("role tools=%v", names)
				}
				if body.MaxTokens != 0 || body.MaxCompletionTokens != 32 || body.ReasoningEffort != "high" || body.Temperature < 0.24 || body.Temperature > 0.26 {
					t.Errorf("model settings not applied: max=%d temp=%f", body.MaxTokens, body.Temperature)
				}
				var text strings.Builder
				for _, message := range body.Messages {
					text.WriteString(message.Content)
				}
				if strings.Contains(text.String(), "PARENT_PRIVATE_HISTORY") || !strings.Contains(text.String(), "ROLE_SYSTEM") || !strings.Contains(text.String(), "HOST_SYSTEM") {
					t.Error("role instruction or isolated context is incorrect")
				}
				call := childCalls.Add(1)
				delta := map[string]any{"role": "assistant", "content": "child complete"}
				finish := "stop"
				if call == 1 && tc.wantRead {
					delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "child-read", "type": "function", "function": map[string]string{"name": "read_workspace", "arguments": "{}"}}}}
					finish = "tool_calls"
				}
				chunk := map[string]any{"id": "role-fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
				encoded, _ := json.Marshal(chunk)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
			}))
			defer provider.Close()
			parent := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 0 {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate-role", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"researcher","prompt":"CHILD_PUBLIC_TASK","description":"read evidence"}`}}}}}), nil
				}
				found := false
				for _, m := range input {
					if m.Role == schema.Tool && strings.Contains(m.Content, "child complete") {
						found = true
					}
				}
				if !found {
					t.Error("child result did not reach parent")
				}
				return textStream("parent complete"), nil
			}}
			underlying := &recordingTool{}
			config := Config{Model: "parent", Instruction: "HOST_SYSTEM", ChatModel: parent, Tools: []tool.BaseTool{underlying}, ToolPolicy: harness.ToolPolicy{Denylist: tc.deny},
				ModelRoutes: map[string]harness.ModelRoute{"research-route": {Provider: "openai", Model: "role-wire-model", BaseURL: provider.URL + "/v1", APIKey: "child-fixture-key"}},
				Subagents:   &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"researcher": {Description: "Read evidence", SystemPrompt: "ROLE_SYSTEM", Model: "research-route", Tools: tc.tools, ReasoningEffort: "high", ModelSettings: harness.AgentModelSettings{Temperature: rolePointer(0.25), MaxTokens: rolePointer(32)}}}},
			}
			e, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			req := request("configured-role-" + tc.name)
			req.History = []harness.Message{{Role: "user", Content: []harness.Content{{Type: "text", Text: "PARENT_PRIVATE_HISTORY"}}}}
			result, err := e.Run(context.Background(), req, nil, allowTool)
			if err != nil || result.StopReason != "end_turn" || childCalls.Load() == 0 {
				t.Fatalf("result=%+v child=%d err=%v", result, childCalls.Load(), err)
			}
			if got := underlying.calls.Load(); (got == 1) != tc.wantRead {
				t.Fatalf("actual child tool calls=%d", got)
			}
		})
	}
}

func TestConfiguredRoleModelCallsShareParentBudget(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "budget-role", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}}}}), nil
		}
		t.Error("child model ran after budget exhaustion")
		return textStream("unexpected"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Budget: harness.BudgetLimits{MaxModelCalls: 1}, Subagents: &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), request("role-budget"), nil, allowTool)
	if err != nil || result.Limit != "model_calls" || fake.calls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestRoleOptionsRespectSmallerOutputCap(t *testing.T) {
	settings := harness.AgentModelSettings{MaxTokens: rolePointer(32)}
	options := roleModelOptions(settings, []model.Option{model.WithMaxTokens(8)})
	if value := model.GetCommonOptions(nil, options...).MaxTokens; value == nil || *value != 8 {
		t.Fatalf("expanded caller token cap: %v", value)
	}
}

func TestConfiguredRoleDurablePermissionReconstruction(t *testing.T) {
	for _, scenario := range []string{"allow", "deny", "changed-role", "removed-role", "host-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call == 2 {
					var system strings.Builder
					for _, msg := range input {
						if msg.Role == schema.System {
							system.WriteString(msg.Content)
						}
					}
					if !strings.Contains(system.String(), "Inspect the workspace") || strings.Contains(system.String(), "Different instructions") {
						t.Error("resumed child lost its original role instructions")
					}
				}
				if scenario == "changed-role" && call == 4 {
					return delegateReaderMessage(), nil
				}
				if scenario == "changed-role" && call == 5 {
					var system strings.Builder
					for _, msg := range input {
						if msg.Role == schema.System {
							system.WriteString(msg.Content)
						}
					}
					if !strings.Contains(system.String(), "Different instructions") {
						t.Error("new task did not use the latest role")
					}
				}
				switch call {
				case 0:
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}}}}), nil
				case 1:
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "child-read", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				default:
					return textStream("done"), nil
				}
			}}
			var effects atomic.Int32
			s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) {
				effects.Add(1)
				return "child effect", nil
			}}, true)
			config := s.Engine.(*Engine).config
			config.Delegations, _ = agents.NewController(native.DB())
			config.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{
				"reader": {Description: "read", SystemPrompt: "Inspect the workspace", Tools: []string{"read_workspace"}},
			}}
			var err error
			s.Engine, err = New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(hr.WithTransportCancellation(context.Background()))
			defer cancel()
			trace := map[string]int{}
			emit := func(_ context.Context, event harness.RunEvent) error { trace[event.Kind]++; return nil }
			result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, emit, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				cancel()
				return harness.PermissionCancelled, context.Canceled
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput || effects.Load() != 0 {
				t.Fatalf("initial result=%+v execution=%+v effects=%d err=%v", result, result.Execution, effects.Load(), err)
			}
			if err = s.Disconnect(context.Background(), "owner"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Load(context.Background(), "next-owner", session.ID, session.CWD, false, nil); err != nil {
				t.Fatal(err)
			}
			if scenario == "changed-role" {
				role := config.Subagents.CustomAgents["reader"]
				role.SystemPrompt = "Different instructions must not enter an existing checkpoint"
				config.Subagents.CustomAgents["reader"] = role
			}
			if scenario == "removed-role" {
				delete(config.Subagents.CustomAgents, "reader")
			}
			if scenario == "host-revoked" {
				config.ToolPolicy.Denylist = []string{"read_workspace"}
			}
			// Reassemble the native agent graph from durable state, as after a
			// daemon restart, retaining only the provider and storage fixtures.
			config.Delegations, err = agents.NewController(native.DB())
			if err != nil {
				t.Fatal(err)
			}
			s.Engine, err = New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			continued, err := s.ResumeExecution(context.Background(), "next-owner", session.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, emit, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				if scenario == "deny" {
					return harness.RejectOnce, nil
				}
				return harness.AllowOnce, nil
			})
			if scenario == "host-revoked" {
				if err == nil || !strings.Contains(err.Error(), "policy changed") || effects.Load() != 0 {
					t.Fatalf("changed policy resumed: effects=%d result=%+v err=%v", effects.Load(), continued, err)
				}
				return
			}
			wantEffects := int32(1)
			if scenario == "deny" {
				wantEffects = 0
			}
			if err != nil || continued.Execution == nil || continued.Execution.Status != harness.ExecutionCompleted || effects.Load() != wantEffects {
				t.Fatalf("resume result=%+v execution=%+v effects=%d err=%v", continued, continued.Execution, effects.Load(), err)
			}
			for _, kind := range []string{"subagent_start", "subagent_suspended", "subagent_resumed", "subagent_end"} {
				if trace[kind] != 1 {
					t.Fatalf("duplicate/missing lifecycle: %v", trace)
				}
			}
			var modelCalls, receipts, grants int
			if err = native.DB().QueryRow(`SELECT model_calls FROM budget_roots`).Scan(&modelCalls); err != nil {
				t.Fatal(err)
			}
			if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_tool_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_execution_grants WHERE state='consumed'`).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if modelCalls != 4 || receipts != 1 || grants != 1 {
				t.Fatalf("models=%d receipts=%d consumed grants=%d", modelCalls, receipts, grants)
			}
			var invocations int
			if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_agent_invocations`).Scan(&invocations); err != nil || invocations != 1 {
				t.Fatalf("resume duplicated delegation: count=%d err=%v", invocations, err)
			}
			if scenario == "changed-role" {
				latest, err := s.Run(context.Background(), "next-owner", session.ID, []harness.Content{{Type: "text", Text: "new task"}}, nil, allowTool)
				if err != nil || latest.Execution == nil || latest.Execution.Status != harness.ExecutionCompleted || fake.calls != 7 {
					t.Fatalf("new role task=%+v calls=%d err=%v", latest, fake.calls, err)
				}
			}
		})
	}
}

func TestConfiguredRoleNestedDelegationAndPermission(t *testing.T) {
	for _, depth := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call < depth {
					role := []string{"planner", "researcher", "reader"}[call]
					args := fmt.Sprintf(`{"subagent_type":%q,"prompt":"inspect","description":"inspect"}`, role)
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprintf("delegate-%d", call), Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: args}}}}}), nil
				}
				if call == depth {
					return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "leaf-read", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
				}
				return textStream("done"), nil
			}}
			var effects atomic.Int32
			s, session, native := runtimeWithDurableInteraction(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "evidence", nil }}, true)
			config := s.Engine.(*Engine).config
			config.Subagents = &harness.SubagentsConfig{MaxConcurrent: rolePointer(1), MaxDepth: rolePointer(depth), CustomAgents: map[string]harness.AgentConfig{
				"planner":    {Description: "plan", SystemPrompt: "plan", DelegateTo: []string{"researcher"}, Tools: []string{"task", "read_workspace"}},
				"researcher": {Description: "research", SystemPrompt: "research", DelegateTo: []string{"reader"}, Tools: []string{"task", "read_workspace"}},
				"reader":     {Description: "read", SystemPrompt: "read", Tools: []string{"read_workspace"}},
			}}
			var err error
			s.Engine, err = New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			// Nested SQLite checkpoint reconstruction is slower with race instrumentation.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			trace := map[string]int{}
			result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, func(_ context.Context, e harness.RunEvent) error { trace[e.Kind]++; return nil }, approveReceiptTool)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || effects.Load() != 1 {
				t.Fatalf("nested %d result=%+v effects=%d err=%v", depth, result, effects.Load(), err)
			}
			for _, kind := range []string{"subagent_start", "subagent_suspended", "subagent_resumed", "subagent_end"} {
				if trace[kind] != depth {
					t.Fatalf("depth=%d trace=%v", depth, trace)
				}
			}
			var models int
			if err = native.DB().QueryRow(`SELECT model_calls FROM budget_roots`).Scan(&models); err != nil {
				t.Fatal(err)
			}
			if models != 2*(depth+1) {
				t.Fatalf("depth=%d model calls=%d", depth, models)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			// Each model call binds its own tool set. The deepest configured
			// agent must not advertise further delegation or parent-only tools.
			if len(fake.tools) <= depth || slices.Contains(fake.tools[depth], "task") || slices.Contains(fake.tools[depth], "background_agent") {
				t.Fatalf("depth boundary tools=%v", fake.tools)
			}
		})
	}
}

func TestConfiguredRoleTaskLimitReturnsToParent(t *testing.T) {
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 || call == 2 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprintf("task-%d", call), Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}}}}}), nil
		}
		if call == 3 {
			found := false
			for _, m := range input {
				if m.Role == schema.Tool && strings.Contains(m.Content, "subagent task limit reached") {
					found = true
				}
			}
			if !found {
				t.Error("parent did not receive task limit result")
			}
		}
		if call > 3 {
			t.Error("unexpected extra child execution")
		}
		return textStream("done"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Subagents: &harness.SubagentsConfig{MaxTasksPerRoot: rolePointer(1), CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Run(context.Background(), request("limited-tasks"), nil, allowTool)
	if err != nil || result.StopReason != "end_turn" || fake.calls != 4 {
		t.Fatalf("result=%+v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestConfiguredRolesLimitConcurrentProviderWork(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var active, peak, calls atomic.Int32
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
				}
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"bounded\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			parent := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call > 0 {
					return textStream("done"), nil
				}
				var tasks []schema.ToolCall
				for i := 0; i < 4; i++ {
					tasks = append(tasks, schema.ToolCall{ID: fmt.Sprintf("parallel-%d", i), Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"reader","prompt":"inspect","description":"inspect"}`}})
				}
				return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: tasks}}), nil
			}}
			e, err := New(context.Background(), Config{ChatModel: parent, ModelRoutes: map[string]harness.ModelRoute{"child": {Model: "fixture", BaseURL: provider.URL + "/v1", APIKey: "fixture"}}, Subagents: &harness.SubagentsConfig{MaxConcurrent: rolePointer(limit), CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "read", Model: "child"}}}})
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() {
				_, err := e.Run(ctx, request(fmt.Sprint("parallel-", limit)), nil, allowTool)
				completed <- err
			}()
			for i := 0; i < limit; i++ {
				select {
				case <-entered:
				case <-ctx.Done():
					close(release)
					t.Fatal("provider concurrency did not reach configured limit")
				}
			}
			select {
			case <-entered:
				t.Error("provider exceeded configured concurrency")
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("parallel tasks did not finish")
			}
			if peak.Load() != int32(limit) || calls.Load() != 4 {
				t.Fatalf("peak=%d calls=%d limit=%d", peak.Load(), calls.Load(), limit)
			}
		})
	}
}
