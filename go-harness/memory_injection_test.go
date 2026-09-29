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
