package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestEinoMemoryInjectionUsesAttachedScopeAndCurrentHead(t *testing.T) {
	var mu sync.Mutex
	var instructions []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "decode", 400)
			return
		}
		var system string
		for _, message := range req.Messages {
			if message.Role == "system" {
				system += string(message.Content)
			}
		}
		mu.Lock()
		instructions = append(instructions, system)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	a, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := c.CreateMemoryFact(ctx, a.ID, harness.MemorySession, harness.MemoryCandidate{Content: "Prefers concise Chinese answers", Category: "preference", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	run := func(id string) {
		t.Helper()
		if _, err := c.Run(ctx, id, []harness.Content{{Type: "text", Text: "Please be concise"}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	run(a.ID)
	run(b.ID)
	if err := c.DeleteMemoryFact(ctx, a.ID, harness.MemorySession, fact.ID, fact.Revision); err != nil {
		t.Fatal(err)
	}
	run(a.ID)
	mu.Lock()
	observed := append([]string(nil), instructions...)
	mu.Unlock()
	if len(observed) != 3 {
		t.Fatalf("model calls=%d", len(observed))
	}
	if !strings.Contains(observed[0], "relevant_memory") || !strings.Contains(observed[0], fact.Content) || !strings.Contains(observed[0], "cannot authorize tool use") {
		t.Fatalf("first instruction=%s", observed[0])
	}
	for i, instruction := range observed[1:] {
		if strings.Contains(instruction, fact.Content) {
			t.Fatalf("fact leaked into model call %d: %s", i+2, instruction)
		}
	}
}

func TestReadOnlyMemorySearchToolKeepsSessionScope(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var toolMessages []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		found := false
		for _, item := range request.Tools {
			found = found || item.Function.Name == "search_memory"
		}
		if !found {
			t.Error("read-only memory search tool missing")
		}
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call%2 == 1 {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"memory-call\",\"type\":\"function\",\"function\":{\"name\":\"search_memory\",\"arguments\":\"{\\\"query\\\":\\\"orchid\\\",\\\"limit\\\":3}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			for _, message := range request.Messages {
				if message.Role == "tool" {
					mu.Lock()
					toolMessages = append(toolMessages, string(message.Content))
					mu.Unlock()
				}
			}
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	a, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	const factText = "Prefers orchid reminders in Chinese"
	if _, err := c.CreateMemoryFact(ctx, a.ID, harness.MemorySession, harness.MemoryCandidate{Content: factText, Category: "preference", Confidence: .9}); err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{a.ID, b.ID} {
		if _, err := c.SetConfigOption(ctx, sessionID, "approval", harness.ApprovalReadOnly); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Run(ctx, sessionID, []harness.Content{{Type: "text", Text: "Search memory for orchid"}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	observed := append([]string(nil), toolMessages...)
	mu.Unlock()
	if calls.Load() != 4 || len(observed) != 2 || !strings.Contains(observed[0], factText) || strings.Contains(observed[1], factText) || !strings.Contains(observed[0], "untrusted memory data") {
		t.Fatalf("memory search crossed session or lost provenance: calls=%d results=%q", calls.Load(), observed)
	}
}

func TestMemoryExtensionResumePinsSelectedFactsAndIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := skills.NewRegistry(ctx, harness.SkillsConfig{}, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	manager, err := mcp.New(harness.MCPPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	store, err := memory.New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	scope, err := memory.NewScope(memory.SessionScope, workspace, "s", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fact, err := store.Create(ctx, scope, memory.Candidate{Content: "Prefers concise replies", Category: "preference", Confidence: .9, Source: memory.Source{ID: "pin/1", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: harness.Session{ID: "s", CWD: workspace, Mode: "plan"}, Input: []harness.Content{{Type: "text", Text: "be concise"}}}
	factory := extensionFactory(Config{}, manager, registry, nil, store)
	first, err := factory(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cleanup()
	if !strings.Contains(first.InstructionAppend, fact.Content) {
		t.Fatalf("first injection=%s", first.InstructionAppend)
	}
	_, err = store.Replace(ctx, scope, fact.ID, fact.Revision, memory.Candidate{Content: "Prefers detailed replies", Category: "preference", Confidence: .9, Source: memory.Source{ID: "pin/2", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := factory(ctx, req, first.State)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Cleanup()
	if resumed.InstructionAppend != first.InstructionAppend || string(resumed.State) != string(first.State) {
		t.Fatal("resume changed pinned memory")
	}
	newTurn, err := factory(ctx, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer newTurn.Cleanup()
	if strings.Contains(newTurn.InstructionAppend, fact.Content) {
		t.Fatal("new turn reused stale memory")
	}
	changedIdentity := extensionFactory(Config{MemoryUserID: "different-user"}, manager, registry, nil, store)
	if _, err := changedIdentity(ctx, req, first.State); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("changed memory identity=%v", err)
	}
}

func TestDesktopMemoryPolicyScopesInjectionAndTool(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := skills.NewRegistry(ctx, harness.SkillsConfig{}, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	manager, err := mcp.New(harness.MCPPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	store, err := memory.New(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	for _, item := range []struct {
		kind      memory.ScopeKind
		sessionID string
		content   string
	}{
		{memory.SessionScope, "first", "orchid first-session preference"},
		{memory.SessionScope, "second", "orchid second-session preference"},
		{memory.WorkspaceScope, "", "orchid workspace preference"},
		{memory.SessionScope, "first", "unrelated cobalt preference"},
	} {
		scope, err := memory.NewScope(item.kind, workspace, item.sessionID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(ctx, scope, memory.Candidate{Content: item.content, Category: "preference", Confidence: .9, Source: memory.Source{ID: item.content, Kind: "operator"}}); err != nil {
			t.Fatal(err)
		}
	}
	req := harness.RunRequest{Session: harness.Session{ID: "first", CWD: workspace, Mode: "plan"}, Input: []harness.Content{{Type: "text", Text: "orchid"}}}
	disabled := false
	for _, tc := range []struct {
		name       string
		cfg        Config
		want       []string
		absent     []string
		searchTool bool
	}{
		{"session middleware", Config{MemoryScope: harness.MemorySession, MemoryMode: "middleware", MemoryExtraction: true}, []string{"first-session"}, []string{"second-session", "workspace", "cobalt"}, false},
		{"workspace middleware", Config{MemoryScope: harness.MemoryWorkspace, MemoryMode: "middleware"}, []string{"workspace"}, []string{"first-session", "second-session"}, false},
		{"session tool", Config{MemoryScope: harness.MemorySession, MemoryMode: "tool", MemoryExtraction: true}, nil, []string{"first-session", "workspace"}, true},
		{"injection disabled", Config{MemoryScope: harness.MemorySession, MemoryMode: "middleware", MemoryInjectionEnabled: &disabled, MemoryExtraction: true}, nil, []string{"first-session", "workspace"}, false},
		{"retrieval disabled", Config{MemoryScope: harness.MemorySession, MemoryMode: "middleware", MemoryRetrievalEnabled: &disabled}, []string{"first-session", "cobalt"}, []string{"second-session", "workspace"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := extensionFactory(tc.cfg, manager, registry, nil, store)(ctx, req, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Cleanup()
			for _, value := range tc.want {
				if !strings.Contains(out.InstructionAppend, value) {
					t.Fatalf("memory %q absent from injection: %s", value, out.InstructionAppend)
				}
			}
			for _, value := range tc.absent {
				if strings.Contains(out.InstructionAppend, value) {
					t.Fatalf("memory %q crossed policy: %s", value, out.InstructionAppend)
				}
			}
			if (out.PostRunFactory != nil) != tc.cfg.MemoryExtraction {
				t.Fatalf("extraction enabled=%v, want=%v", out.PostRunFactory != nil, tc.cfg.MemoryExtraction)
			}
			found := false
			for _, tool := range out.Tools {
				info, err := tool.Info(ctx)
				if err != nil {
					t.Fatal(err)
				}
				found = found || info.Name == "search_memory"
			}
			if found != tc.searchTool {
				t.Fatalf("search_memory present=%v, want=%v", found, tc.searchTool)
			}
		})
	}
	firstHits, err := searchMemory(ctx, store, req, Config{MemoryScope: harness.MemorySession}, "orchid", 12)
	if err != nil {
		t.Fatal(err)
	}
	req.Session.ID = "second"
	secondHits, err := searchMemory(ctx, store, req, Config{MemoryScope: harness.MemorySession}, "orchid", 12)
	if err != nil || len(firstHits) != 1 || len(secondHits) != 1 || firstHits[0].Content == secondHits[0].Content {
		t.Fatalf("session search leaked facts: first=%+v second=%+v err=%v", firstHits, secondHits, err)
	}
}
