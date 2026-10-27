package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestPersonalMemoryToolCommitsBeforeAnswerAndNewSessionReadsIt(t *testing.T) {
	var calls atomic.Int32
	var c *Client
	var mu sync.Mutex
	var systemPrompts []string
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
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
		tools := map[string]bool{}
		for _, tool := range request.Tools {
			tools[tool.Function.Name] = true
		}
		if !tools["remember_memory"] || !tools["forget_memory"] || !tools["search_memory"] {
			t.Errorf("memory tools missing in middleware mode: %v", tools)
		}
		var system string
		for _, m := range request.Messages {
			if m.Role == "system" {
				system += string(m.Content)
			}
		}
		mu.Lock()
		systemPrompts = append(systemPrompts, system)
		mu.Unlock()
		call := calls.Add(1)
		message := map[string]any{"role": "assistant", "content": "已记住"}
		reason := "stop"
		if call == 1 {
			args := `{"scope":"global","key":"profile.last_reported_location","category":"profile","content":"用户最近告知所在地是北京。","evidence":"我在北京","expected_revision":0}`
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "remember-1", "type": "function", "function": map[string]any{"name": "remember_memory", "arguments": args}}}}
			reason = "tool_calls"
		} else if call == 2 {
			scope, _ := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
			page, err := c.service.Memory.List(context.Background(), scope, "", 100)
			if err != nil || len(page.Facts) != 1 {
				t.Errorf("answer before commit: %+v %v", page, err)
			}
			found := false
			for _, m := range request.Messages {
				if m.Role == "tool" && strings.Contains(string(m.Content), "saved") {
					found = true
				}
			}
			if !found {
				t.Error("no committed receipt in tool result")
			}
		}
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}})
	}))
	defer provider.Close()
	ctx := context.Background()
	var err error
	c, err = Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemorySingleUser: true, MemoryMode: "middleware", PermissionMode: harness.PermissionModeOff})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, a.ID, []harness.Content{{Type: "text", Text: "请记住，我在北京"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A poll that finds only explicit turns must still commit its queue cleanup.
	_, err = c.store.DB().Exec(`INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,status,available_at,updated_at)
SELECT 'explicit-job',source_run_id,source_input_id,'fixture','{}','queued','2000-01-01','2000-01-01' FROM memory_fact_revisions WHERE policy_version='personal-explicit-v1' LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.service.Memory.ClaimJob(ctx)
	var status string
	if err = c.store.DB().QueryRow(`SELECT status FROM memory_jobs WHERE id='explicit-job'`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("explicit queue cleanup rolled back: %s %v", status, err)
	}
	b, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, b.ID, []harness.Content{{Type: "text", Text: "你知道我在哪吗"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(systemPrompts) != 3 || !strings.Contains(systemPrompts[2], "用户最近告知所在地是北京") {
		t.Fatalf("new session failed to read personal memory: %q", systemPrompts)
	}
}

func TestPersonalMemoryBackgroundQueueSurvivesClientRestart(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		text := "done"
		extract := false
		for _, m := range request.Messages {
			extract = extract || strings.Contains(string(m.Content), "Extract at most 4")
		}
		if extract && fail.Load() {
			http.Error(w, "temporarily unavailable", 503)
			return
		}
		if extract {
			text = `{"facts":[{"scope":"global","key":"city","category":"profile","content":"用户最近告知所在地是北京。","evidence":"我在北京"},{"scope":"global","key":"preference.language","category":"preference","content":"用户偏好中文。","evidence":"偏好中文"}]}`
		}
		_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	}))
	defer provider.Close()
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemorySingleUser: true, MemoryExtraction: true}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, a.ID, []harness.Content{{Type: "text", Text: "我在北京，偏好中文"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM memory_jobs`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("durable job missing: %d %v", pending, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Remove only the fixture's backoff, as if its retry deadline elapsed.
	if _, err = c.store.DB().Exec(`UPDATE memory_jobs SET available_at='2000-01-01' WHERE status='queued'`); err != nil {
		t.Fatal(err)
	}
	scope, _ := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.service.Memory.List(ctx, scope, "", 100)
		if err == nil && len(p.Facts) == 2 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	status, _ := c.service.Memory.JobStatus(ctx)
	t.Fatalf("job did not restore both facts: %+v", status)
}

func TestPersonalMemoryManagementDurableReceiptAndProfileGuard(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", MemorySingleUser: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(fields map[string]any) (any, error) {
		raw, _ := json.Marshal(fields)
		var f map[string]json.RawMessage
		_ = json.Unmarshal(raw, &f)
		return c.ManageLocal(ctx, localhost.ManagementRequest{Operation: fields["operation"].(string), Fields: f})
	}
	operation := map[string]any{"operation": "memory.personal.mutate", "profile_id": c.service.MemoryGlobalID, "operation_id": "phone-1", "action": "remember", "key": "profile.name", "content": "用户昵称是小鹿。", "category": "profile", "expected_revision": 0}
	first, err := call(operation)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := call(operation)
	if err != nil || fmt.Sprint(replay) != fmt.Sprint(first) {
		t.Fatalf("replay=%v %v", replay, err)
	}
	status, err := call(map[string]any{"operation": "memory.personal.receipt", "operation_id": "phone-1"})
	if err != nil || status.(map[string]any)["found"] != true {
		t.Fatalf("status=%v %v", status, err)
	}
	operation["profile_id"] = "wrong-profile"
	if _, err = call(operation); err == nil {
		t.Fatal("foreign profile mutation accepted")
	}
	operation["profile_id"] = c.service.MemoryGlobalID
	operation["automatic"] = true
	if _, err = call(operation); err == nil {
		t.Fatal("client supplied automatic authority accepted")
	}
}
