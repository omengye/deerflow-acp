package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestTaskContextNativeStreamingCompactionAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	store, err := hr.NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Append(ctx, harness.RunEvent{SessionID: session.ID, Kind: "user_message", Content: []harness.Content{{Type: "text", Text: "ARCHIVED_REQUIREMENT do not change the API"}}})
	if err != nil {
		t.Fatal(err)
	}
	steps, summaries := 0, 0
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(_ context.Context, _ int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if len(messages) > 0 && (strings.Contains(messages[0].Content, "tasked with summarizing conversations") || strings.Contains(messages[0].Content, "负责总结对话")) {
			summaries++
			return textStream("Earlier work summarized."), nil
		}
		if steps > 0 {
			found := false
			for _, m := range messages {
				if strings.Contains(m.Content, "Task notebook:") && strings.Contains(m.Content, "KEEP_API") {
					found = true
				}
			}
			if !found {
				t.Error("notebook absent after tool write/compaction/reopen")
			}
		}
		steps++
		name, args := "task_note", `{"action":"set","key":"constraint","content":"KEEP_API","expected_revision":0}`
		switch {
		case steps == 1:
		case steps == 2:
			name, args = "history_search", `{"query":"ARCHIVED_REQUIREMENT","role":"user"}`
		case steps == 3:
			name, args = "history_read", fmt.Sprintf(`{"source_id":%d}`, old.Sequence)
		case steps < 8:
			args = `{"action":"list"}`
		default:
			return textStream("done"), nil
		}
		return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: fmt.Sprint(steps), Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}})}), nil
	}}}
	makeEngine := func() *Engine {
		return newTestEngine(t, Config{ChatModel: fake, Model: "fixture", TaskContextStore: store, SessionStore: db, CheckpointStore: db, PermissionMode: harness.PermissionModeOff, Compaction: harness.CompactionConfig{Enabled: true, ContextMessages: 6, KeepRecentMessages: 2}})
	}
	e := makeEngine()
	run := func(id string) {
		req := harness.RunRequest{Session: session, RunID: id, InputID: "input-" + id, Input: []harness.Content{{Type: "text", Text: "continue task"}}}
		if err := store.BeginRun(ctx, req); err != nil {
			t.Fatal(err)
		}
		result, err := e.Run(ctx, req, func(ctx context.Context, event harness.RunEvent) error {
			_, err := store.Append(ctx, event)
			return err
		}, nil)
		if err != nil || result.StopReason != "end_turn" {
			t.Fatalf("%+v %v", result, err)
		}
		if err = store.Finish(ctx, req.RunID, result.StopReason, nil); err != nil {
			t.Fatal(err)
		}
	}
	run("first")
	if summaries < 1 {
		t.Fatal("compaction was not exercised")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err = hr.NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	e = makeEngine()
	run("second")
	for _, input := range fake.seen {
		count := 0
		for _, m := range input {
			if strings.Contains(m.Content, "Task notebook:") {
				count++
			}
		}
		if count > 1 {
			t.Fatal("notebook persisted into conversation history")
		}
	}
}

func TestTaskContextPolicyDeniesToolsAndInjection(t *testing.T) {
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("ok"), nil
	}}}
	store := &deniedTaskContext{}
	e := newTestEngine(t, Config{ChatModel: fake, TaskContextStore: store, ToolPolicy: harness.ToolPolicy{Allowlist: []string{}}, PermissionMode: harness.PermissionModeOff})
	if _, err := e.Run(context.Background(), request("denied-context"), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, names := range fake.tools {
		for _, name := range names {
			if name == "task_note" || name == "history_search" || name == "history_read" {
				t.Fatal("policy bypass", name)
			}
		}
	}
}

func TestTaskContextDoesNotLeakIntoDelegatedRole(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := hr.NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: session, RunID: "isolation", InputID: "isolation-input", Input: []harness.Content{{Type: "text", Text: "delegate"}}}
	if err = store.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetTaskNote(ctx, req, "private", "PARENT_NOTE_PRIVATE", 0); err != nil {
		t.Fatal(err)
	}
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(_ context.Context, n int, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if n == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{{ID: "delegate", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","description":"inspect","prompt":"read only your own prompt"}`}}})}), nil
		}
		if n == 1 {
			for _, m := range messages {
				if strings.Contains(m.Content, "PARENT_NOTE_PRIVATE") {
					t.Error("parent notebook leaked to child")
				}
			}
		}
		return textStream("done"), nil
	}}}
	e, err := New(ctx, Config{ChatModel: fake, Model: "fixture", TaskContextStore: store, PermissionMode: harness.PermissionModeOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Run(ctx, req, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.tools) < 3 {
		t.Fatalf("delegation not exercised: %v", fake.tools)
	}
	for _, name := range fake.tools[1] {
		if name == "task_note" || name == "history_search" || name == "history_read" {
			t.Fatal("parent history tool leaked", name)
		}
	}
}

type deniedTaskContext struct{}

func (*deniedTaskContext) TaskNotes(context.Context, harness.RunRequest) ([]harness.TaskNote, error) {
	panic("denied notebook was read")
}
func (*deniedTaskContext) SetTaskNote(context.Context, harness.RunRequest, string, string, int64) (harness.TaskNote, error) {
	panic("denied notebook was written")
}
func (*deniedTaskContext) SearchTaskHistory(context.Context, harness.RunRequest, string, string, int64) (harness.TaskHistoryPage, error) {
	panic("denied history was searched")
}
func (*deniedTaskContext) ReadTaskHistory(context.Context, harness.RunRequest, int64, int) (harness.TaskHistoryEntry, error) {
	panic("denied history was read")
}

func TestTaskContextGenerateCollectsStream(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := hr.NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: session, RunID: "sync", InputID: "sync-input"}
	if err = store.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	fake := &noGenerateModel{scriptedModel: &scriptedModel{stream: func(context.Context, int, []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		return textStream("stream-only"), nil
	}}}
	m := &taskContextModel{inner: fake, store: store, req: req}
	got, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("hello")})
	if err != nil || got.Content != "stream-only" {
		b, _ := json.Marshal(got)
		t.Fatal(string(b), err)
	}
	e := newTestEngine(t, Config{ChatModel: fake, TaskContextStore: store, PermissionMode: harness.PermissionModeOff})
	p, err := e.PrepareAttempt(ctx, req, "deerflow", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.JoinAndClose(ctx)
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: p.Agent})
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage("synchronous runner")})
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	if err = p.JoinAndClose(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 {
		t.Fatalf("unexpected request count: %d", fake.calls)
	}
}
