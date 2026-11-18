package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestPersonalMemoryMutationPreservesExecutionResume(t *testing.T) {
	for _, test := range []struct {
		name, action string
		reopen       bool
	}{
		{name: "remember", action: "remember"},
		{name: "forget", action: "forget"},
		{name: "remember_reopen", action: "remember", reopen: true},
		{name: "forget_reopen", action: "forget", reopen: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := test.action
			const oldFact = "The user's hardware is OLD-BOARD."
			const newFact = "The user's hardware is NEW-BOARD."
			prompt := "Please remember my hardware is NEW-BOARD"
			if action == "forget" {
				prompt = "Please forget my hardware"
			}
			var calls atomic.Int32
			provider := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				call := calls.Add(1)
				var system string
				for _, m := range request.Messages {
					if m.Role == "system" {
						system += string(m.Content)
					}
				}
				if call == 1 && !strings.Contains(system, oldFact) {
					t.Error("fixture did not inject the original memory")
				}
				if call >= 2 && strings.Contains(system, oldFact) {
					t.Error("resume injected a corrected or forgotten memory")
				}
				name, args := "", map[string]any{}
				switch call {
				case 1:
					name = action + "_memory"
					args = map[string]any{"scope": "global", "key": "profile.hardware", "category": "profile", "expected_revision": 1, "evidence": prompt, "evidence_start": 0, "evidence_end": len([]rune(prompt))}
					if action == "remember" {
						args["content"] = newFact
						args["value"] = "NEW-BOARD"
					}
				case 2:
					name = "write_todos"
					args = map[string]any{"todos": []any{map[string]string{"content": "Write the fixture result", "status": "in_progress"}}}
				case 3:
					name = "write_file"
					args = map[string]any{"path": "result.txt", "content": "completed once"}
				}
				message, reason := map[string]any{"role": "assistant", "content": "Finished"}, "stop"
				if name != "" {
					encoded, _ := json.Marshal(args)
					message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": name + "-once", "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}}}}
					reason = "tool_calls"
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}})
			}))
			defer provider.Close()
			ctx := context.Background()
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", MemorySingleUser: true, DisableSubagents: true, PermissionMode: harness.PermissionModeAll}
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			workspace := t.TempDir()
			sess, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
			if err != nil {
				t.Fatal(err)
			}
			seed, err := c.service.Memory.Mutate(ctx, scope, memory.Mutation{OperationID: "seed-hardware", Action: "remember", Key: "profile.hardware", Content: oldFact, Category: "profile", Source: memory.Source{ID: "seed-hardware", Kind: "operator"}})
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			approve := func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				approvals++
				return harness.AllowOnce, nil
			}
			result, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, func(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
				if test.reopen && req.ToolName == "write_todos" {
					return harness.PermissionCancelled, nil
				}
				return approve(ctx, req)
			})
			if test.reopen {
				if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
					t.Fatalf("pause before reconnect: %+v %v", result, err)
				}
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				c = nil
				c, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				state, err := c.Execution(ctx, sess.ID, result.Execution.RunID)
				if err != nil || !state.Resumable || calls.Load() != 2 {
					t.Fatalf("reconnect state=%+v calls=%d err=%v", state, calls.Load(), err)
				}
				result, err = c.ResumeExecution(ctx, sess.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, approve)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || result.Execution.Attempt != 4 {
				t.Fatalf("memory mutation blocked later tool resumes: result=%+v err=%v", result, err)
			}
			if calls.Load() != 4 || approvals != 3 {
				t.Fatalf("unexpected replay: model calls=%d approvals=%d", calls.Load(), approvals)
			}
			fact, err := c.service.Memory.Get(ctx, scope, seed.FactID)
			if action == "remember" {
				if err != nil || fact.Revision != 2 || fact.Content != newFact {
					t.Fatalf("memory update lost or replayed: %+v %v", fact, err)
				}
			} else if !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("forgotten memory remains: %+v %v", fact, err)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
			if err != nil || string(data) != "completed once" {
				t.Fatalf("subsequent tool did not finish: %q %v", data, err)
			}
			receipts, err := c.ListToolReceipts(ctx, sess.ID)
			if err != nil || len(receipts) != 3 {
				t.Fatalf("receipts=%+v err=%v", receipts, err)
			}
			for _, receipt := range receipts {
				if receipt.State != harness.ReceiptCompleted {
					t.Fatalf("unfinished tool: %+v", receipt)
				}
			}
		})
	}
}
