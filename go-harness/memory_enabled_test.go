package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDisabledModelMemoryPreservesFactsForReenable(t *testing.T) {
	var mu sync.Mutex
	type observation struct {
		System string
		Tools  []string
	}
	var seen []observation
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
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		var item observation
		for _, message := range request.Messages {
			if message.Role == "system" {
				item.System += string(message.Content)
			}
		}
		for _, tool := range request.Tools {
			item.Tools = append(item.Tools, tool.Function.Name)
		}
		mu.Lock()
		seen = append(seen, item)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx := context.Background()
	disabled := false
	base := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemoryEnabled: &disabled, MemoryExtraction: true}
	client, err := Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	session, err := client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := client.CreateMemoryFact(ctx, session.ID, harness.MemoryWorkspace, harness.MemoryCandidate{Content: "The user's favorite flower is orchid", Category: "preference", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Which flower?"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if page, err := client.MemoryFacts(ctx, session.ID, harness.MemoryWorkspace, "", 10); err != nil || len(page.Facts) != 1 || page.Facts[0].ID != fact.ID {
		t.Fatalf("disabled model memory lost stored facts: %+v err=%v", page, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first := append([]observation(nil), seen...)
	mu.Unlock()
	if len(first) != 1 || strings.Contains(first[0].System, fact.Content) {
		t.Fatalf("disabled memory leaked into model or made extraction call: %+v", first)
	}
	for _, name := range first[0].Tools {
		if name == "search_memory" {
			t.Fatalf("disabled memory still exposed search_memory: %+v", first)
		}
	}
	base.MemoryEnabled = nil
	base.MemoryExtraction = false
	client, err = Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err = client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Which flower?"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second := append([]observation(nil), seen...)
	mu.Unlock()
	if len(second) != 2 || !strings.Contains(second[1].System, fact.Content) {
		t.Fatalf("reenabled memory did not retrieve stored fact: %+v", second)
	}
	found := false
	for _, name := range second[1].Tools {
		found = found || name == "search_memory"
	}
	if !found {
		t.Fatalf("reenabled memory did not expose search_memory: %+v", second[1])
	}
}
