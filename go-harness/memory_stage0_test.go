package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

// Stage-0 fixtures use real runtime schemas and execution paths, a private
// database, and an in-process streaming provider. No installed state or paid
// model endpoint is used. The memory worker is joined before any test input.
func memoryStage0Client(t *testing.T, cfg Config, handler http.HandlerFunc) (*Client, harness.Session) {
	t.Helper()
	c, session, _ := memoryStage0ConfiguredClient(t, cfg, handler)
	return c, session
}

func memoryStage0ConfiguredClient(t *testing.T, cfg Config, handler http.HandlerFunc) (*Client, harness.Session, Config) {
	t.Helper()
	provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, handler))
	t.Cleanup(provider.Close)
	cfg.DataDir, cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL = t.TempDir(), "openai", "fixture", "fixture", provider.URL+"/v1"
	cfg.DisableSubagents = true
	c, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close stage-0 client: %v", err)
		}
	})
	if c.memoryCancel != nil {
		c.memoryCancel()
		select {
		case <-c.memoryDone:
		case <-time.After(5 * time.Second):
			t.Fatal("fixture could not join the memory worker")
		}
	}
	session, err := c.NewSession(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c, session, cfg
}

func memoryStage0Completion(t *testing.T, w http.ResponseWriter, content, toolName string, args map[string]any) {
	t.Helper()
	message := map[string]any{"role": "assistant", "content": content}
	reason := "stop"
	if toolName != "" {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture encoding error", http.StatusInternalServerError)
			return
		}
		message = map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "stage0-" + toolName, "type": "function", "function": map[string]any{"name": toolName, "arguments": string(raw)}},
		}}
		reason = "tool_calls"
	}
	if err := modelstreamtest.WriteCompletion(w, map[string]any{
		"id": "stage0", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}},
		"usage":   map[string]int{"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24},
	}); err != nil {
		t.Error(err)
	}
}

func memoryStage0ExtractionReply(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var request struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Error(err)
		http.Error(w, "fixture decoding error", http.StatusBadRequest)
		return
	}
	content := "done"
	for _, m := range request.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Extract only durable facts") {
			raw, err := json.Marshal(map[string]any{"schema_version": 2, "facts": []any{map[string]any{
				"scope": "workspace", "durability": "durable", "authority": "descriptive",
				"category": "preference", "content": "User prefers concise Chinese answers", "confidence": .92,
				"key": "preference.answer_style", "value": "Chinese", "evidence": "From now on, please answer concisely in Chinese", "evidence_start": 0, "evidence_end": 47,
			}}})
			if err != nil {
				t.Error(err)
				return
			}
			content = string(raw)
		}
	}
	memoryStage0Completion(t, w, content, "", nil)
}

// F2 must not turn a failed terminal transaction into a successful run. This
// protection stays in the default suite while the opt-in reproductions are red.
func TestMemoryStage0TerminalMemoryFailureRemainsAtomic(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		memoryStage0ExtractionReply(t, w, r)
	})
	_, err := c.store.DB().Exec("CREATE TRIGGER stage0_terminal_failure BEFORE UPDATE ON memory_extraction_audit WHEN NEW.status='promoted' BEGIN SELECT RAISE(ABORT,'stage0_terminal_failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	_, runErr := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "From now on, please answer concisely in Chinese"}}, nil, nil)
	if runErr == nil || !strings.Contains(runErr.Error(), "stage0_terminal_failure") {
		t.Fatalf("fixture must reach the terminal memory transaction and expose its failure: %v", runErr)
	}
	var facts, completed, staged int
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_facts WHERE deleted=0").Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM harness_runs WHERE status='completed'").Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_staged_facts WHERE status='staged'").Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if facts != 0 || completed != 0 || staged != 1 {
		t.Fatalf("terminal failure must roll back fact publication and completed state: facts=%d completed=%d staged=%d", facts, completed, staged)
	}
}
