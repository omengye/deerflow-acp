package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type resourceFixture struct {
	config         Config
	server         harness.MCPServer
	calls, effects atomic.Int32
	blocked        chan struct{}
	remote         *sdk.Server
	blockEffects   atomic.Bool
	effectStarted  chan struct{}
}

func (f *resourceFixture) setTool(description string) {
	sdk.AddTool(f.remote, &sdk.Tool{Name: "record", Description: description}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct {
		Value string `json:"value"`
	}) (*sdk.CallToolResult, any, error) {
		f.effects.Add(1)
		if f.blockEffects.Load() {
			select {
			case f.effectStarted <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "effect recorded"}}}, nil, nil
	})
}

func newResourceFixture(t *testing.T) *resourceFixture {
	t.Helper()
	f := &resourceFixture{blocked: make(chan struct{}), effectStarted: make(chan struct{}, 1)}
	server := sdk.NewServer(&sdk.Implementation{Name: "resource-fixture", Version: "1"}, nil)
	f.remote = server
	f.setTool("Record one external effect")
	mcpServer := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(mcpServer.Close)
	f.server = harness.MCPServer{Name: "fixture", Transport: "http", URL: mcpServer.URL, Headers: map[string]string{"X-Test-Secret": "resource-secret-never-persist"}}
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		child, hasTool := false, false
		for _, message := range request.Messages {
			child = child || message.Role == "user" && strings.Contains(string(message.Content), "RESOURCE_CHILD")
			hasTool = hasTool || message.Role == "tool"
		}
		name, id, args := "background_agent", "delegate", `{"instruction":"RESOURCE_CHILD execute one MCP effect","description":"resource child"}`
		if child {
			call := f.calls.Add(1)
			if call == 2 {
				if !hasTool {
					t.Error("saved MCP result missing before blocked model")
				}
				close(f.blocked)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			name = ""
			for _, item := range request.Tools {
				if strings.HasPrefix(item.Function.Name, "mcp_fixture_record_") {
					name = item.Function.Name
				}
			}
			if name == "" {
				t.Error("child lost its pinned MCP tools")
			}
			id, args = "resource-effect", `{"value":"once"}`
		}
		message := map[string]any{"role": "assistant", "content": "Finished"}
		finish := "stop"
		if !hasTool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if !hasTool {
			raw, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, raw)
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Finished\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":10,\"total_tokens\":20}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)
	t.Cleanup(func() { close(release) })
	f.config = Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, MCP: harness.MCPPolicy{AllowHTTP: true}}
	return f
}

func resourceDelegation(t *testing.T, ctx context.Context, c *Client, f *resourceFixture) (harness.Session, harness.BackgroundTask) {
	t.Helper()
	session, err := c.NewSession(ctx, t.TempDir(), f.server)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delegate this work"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("parent=%+v err=%v", result, err)
	}
	tasks, err := c.BackgroundTasks(ctx, session.ID, "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	return session, tasks[0]
}

func resourceTaskSettled(t *testing.T, ctx context.Context, c *Client, session string, task harness.BackgroundTask) harness.BackgroundTask {
	t.Helper()
	for task.Status == "pending" || task.Status == "running" {
		var err error
		task, err = c.WaitBackgroundTask(ctx, session, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func TestSDKMCPResourceDisconnectDrainAndExplicitAuthorization(t *testing.T) {
	f := newResourceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, err := Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	session, task := resourceDelegation(t, ctx, c, f)
	task = resourceTaskSettled(t, ctx, c, session.ID, task)
	if task.Status != "waiting_input" || task.Interaction == nil || task.Resources == nil || task.Resources.Status != "ready" {
		t.Fatalf("tool approval=%+v", task)
	}
	if _, err = c.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = c.CloseSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, session.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	task, err = c.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil || task.Status != "suspended" || task.Resources == nil || task.Resources.Status != "waiting_connection" || f.effects.Load() != 1 {
		t.Fatalf("disconnected=%+v effects=%d err=%v", task, f.effects.Load(), err)
	}
	if _, err = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("resume absent resource: %v", err)
	}
	incompatible := f.server
	incompatible.Headers = map[string]string{"X-Test-Secret": "different-private-value"}
	if _, err = c.LoadSession(ctx, session.ID, session.CWD, false, nil, incompatible); err != nil {
		t.Fatal(err)
	}
	task, err = c.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil || task.Resources.Status != "incompatible" || task.Resources.CanAuthorize {
		t.Fatalf("changed resource identity=%+v err=%v", task, err)
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, session.ID, task.ID, task.Version, task.Resources.Version); !errors.Is(err, harness.ErrResourcesIncompatible) {
		t.Fatalf("authorized different client resources: %v", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, session.CWD, false, nil, f.server); err != nil {
		t.Fatal(err)
	}
	task, err = c.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil || task.Resources.Status != "waiting_authorization" || !task.Resources.CanAuthorize {
		t.Fatalf("reattached=%+v err=%v", task, err)
	}
	if _, err = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("reattach granted old task: %v", err)
	}
	actor := harness.TaskActor{OwnerID: c.owner, SessionID: session.ID}
	binding, err := c.background.service.BindingForTask(ctx, actor, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, spec, err := c.background.specs.Load(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = c.background.checkContinuationResources(ctx, tx, binding, spec)
	_ = tx.Rollback()
	if !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("continuation ignored resource wait: %v", err)
	}
	other, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, other.ID, task.ID, task.Version, task.Resources.Version); err == nil {
		t.Fatal("other root session authorized resources")
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, session.ID, task.ID, task.Version-1, task.Resources.Version); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale task authorization: %v", err)
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, session.ID, task.ID, task.Version, task.Resources.Version+1); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale resource authorization: %v", err)
	}
	task, err = c.AuthorizeBackgroundResources(ctx, session.ID, task.ID, task.Version, task.Resources.Version)
	if err != nil || task.Resources.Status != "ready" {
		t.Fatalf("authorize=%+v err=%v", task, err)
	}
	tx, err = c.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := c.background.continuationResourceGeneration(ctx, tx, binding, spec)
	_ = tx.Rollback()
	ref, refErr := resourceRef(spec.Extension)
	if err != nil || refErr != nil || generation == "" || generation == ref.OriginGeneration {
		t.Fatalf("continuation did not take explicit replacement grant: generation=%q err=%v refErr=%v", generation, err, refErr)
	}
	task, err = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version)
	if err != nil {
		t.Fatal(err)
	}
	task = resourceTaskSettled(t, ctx, c, session.ID, task)
	if task.Status != "completed" || f.effects.Load() != 1 || f.calls.Load() != 3 {
		t.Fatalf("resumed=%+v effects=%d models=%d", task, f.effects.Load(), f.calls.Load())
	}
	receipts, err := c.service.Store.ListToolReceipts(ctx, task.ChildSessionID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptCompleted {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	var data []byte
	if err = c.store.DB().QueryRowContext(ctx, "SELECT ref FROM harness_background_resources WHERE task_id=?", task.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), f.server.URL) || strings.Contains(string(data), "resource-secret-never-persist") {
		t.Fatal("resource descriptor persisted client credentials")
	}
	var note harness.BackgroundNotification
	for note.ID == "" {
		notes, err := c.BackgroundNotifications(ctx, session.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range notes {
			if candidate.TaskID == task.ID && candidate.Kind == "completed" {
				note = candidate
			}
		}
		if note.ID == "" {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	var attemptsBefore, attemptsAfter, sourceCount int
	if err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM budget_attempts`).Scan(&attemptsBefore); err != nil {
		t.Fatal(err)
	}
	f.setTool("Changed schema before foreground continuation admission")
	if _, err = c.ProcessBackgroundNotification(ctx, session.ID, note.ID, nil, nil); !errors.Is(err, harness.ErrResourcesIncompatible) {
		t.Fatalf("continuation accepted changed tool contract: %v", err)
	}
	if err = c.store.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM budget_attempts),(SELECT count(*) FROM harness_continuation_sources)`).Scan(&attemptsAfter, &sourceCount); err != nil || attemptsAfter != attemptsBefore || sourceCount != 0 {
		t.Fatalf("incompatible continuation consumed admission: attempts %d→%d sources=%d err=%v", attemptsBefore, attemptsAfter, sourceCount, err)
	}
	f.setTool("Record one external effect")
	continued, err := c.ProcessBackgroundNotification(ctx, session.ID, note.ID, nil, nil)
	if err != nil || continued.Execution == nil || continued.Execution.Status != harness.ExecutionCompleted || f.effects.Load() != 1 {
		t.Fatalf("compatible continuation=%+v effects=%d err=%v", continued, f.effects.Load(), err)
	}
}

func TestSDKMCPQueuedDisconnectDoesNotSpendAttempt(t *testing.T) {
	f := newResourceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, err := Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	first, task := resourceDelegation(t, ctx, c, f)
	task = resourceTaskSettled(t, ctx, c, first.ID, task)
	if task.Interaction == nil {
		t.Fatalf("first task=%+v", task)
	}
	if _, err = c.ApproveBackgroundTask(ctx, first.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second, pending := resourceDelegation(t, ctx, c, f)
	if pending.Status != "pending" || pending.Attempt != 0 {
		t.Fatalf("queued=%+v", pending)
	}
	if err = c.CloseSession(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, second.ID, second.CWD, false, nil, f.server); err != nil {
		t.Fatal(err)
	}
	time.Sleep(550 * time.Millisecond)
	pending, err = c.BackgroundTask(ctx, second.ID, pending.ID)
	if err != nil || pending.Status != "pending" || pending.Attempt != 0 || pending.Resources.Status != "waiting_authorization" {
		t.Fatalf("ungranted pending=%+v err=%v", pending, err)
	}
	var attempts int
	if err = c.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM budget_attempts WHERE member_id=?", pending.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("queued task spent %d attempts", attempts)
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, second.ID, pending.ID, pending.Version, pending.Resources.Version); err != nil {
		t.Fatal(err)
	}
	pending = resourceTaskSettled(t, ctx, c, second.ID, pending)
	if pending.Status != "waiting_input" || pending.Attempt != 1 {
		t.Fatalf("authorized queued=%+v", pending)
	}
}

func TestSDKMCPSchemaChangeBeforeDispatchDoesNotSpendAttempt(t *testing.T) {
	f := newResourceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, err := Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	first, task := resourceDelegation(t, ctx, c, f)
	task = resourceTaskSettled(t, ctx, c, first.ID, task)
	if task.Interaction == nil {
		t.Fatalf("first task=%+v", task)
	}
	if _, err = c.ApproveBackgroundTask(ctx, first.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second, pending := resourceDelegation(t, ctx, c, f)
	generation, err := c.mcp.Generation(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.setTool("Changed contract while the same MCP connection remains attached")
	if err = c.CloseSession(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	pending, err = c.BackgroundTask(ctx, second.ID, pending.ID)
	if err != nil || pending.Status != "pending" || pending.Attempt != 0 || pending.Resources == nil || pending.Resources.Status != "incompatible" || pending.Resources.CanAuthorize {
		t.Fatalf("schema mismatch claimed queued task=%+v err=%v", pending, err)
	}
	current, err := c.mcp.Generation(ctx, second.ID)
	if err != nil || current != generation {
		t.Fatalf("fixture accidentally changed generation: %q %q %v", generation, current, err)
	}
	var attempts int
	if err = c.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM budget_attempts WHERE member_id=?", pending.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("schema wait consumed budget attempts=%d err=%v", attempts, err)
	}
}

func TestSDKMCPDisconnectPreservesUncertainExternalEffect(t *testing.T) {
	f := newResourceFixture(t)
	f.blockEffects.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, err := Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	session, task := resourceDelegation(t, ctx, c, f)
	task = resourceTaskSettled(t, ctx, c, session.ID, task)
	if task.Interaction == nil {
		t.Fatalf("approval=%+v", task)
	}
	if _, err = c.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.effectStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = c.CloseSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, session.CWD, false, nil, f.server); err != nil {
		t.Fatal(err)
	}
	task, err = c.BackgroundTask(ctx, session.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := c.service.Store.ListToolReceipts(ctx, task.ChildSessionID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
		t.Fatalf("external effect certainty lost: task=%+v receipts=%+v err=%v", task, receipts, err)
	}
	if task.Resources == nil {
		t.Fatalf("replacement binding=%+v", task)
	}
	if task.Status == "running" && task.Resources.CanAuthorize {
		t.Fatal("unsettled resource grant was presented as authorizable")
	}
	if _, err = c.AuthorizeBackgroundResources(ctx, session.ID, task.ID, task.Version, task.Resources.Version); err != nil && !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("unexpected reconciliation admission error: %v", err)
	}
	if _, err = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version); err == nil {
		t.Fatal("resource authorization bypassed uncertain receipt review")
	}
	time.Sleep(350 * time.Millisecond)
	if f.effects.Load() != 1 {
		t.Fatalf("uncertain effect replayed %d times", f.effects.Load())
	}
}
