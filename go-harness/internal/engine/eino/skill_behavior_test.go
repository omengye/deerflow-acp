package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

func behaviorInput() harness.SkillBehaviorInput {
	return harness.SkillBehaviorInput{EvaluationID: "behavior-eval", CaseID: "calc", Variant: "candidate", Version: strings.Repeat("b", 64), Repetition: 1, Files: map[string][]byte{"SKILL.md": []byte("Follow the task; use the candidate calculation.")}, Case: harness.SkillBehaviorCase{ID: "calc", Kind: "targeted", Prompt: "Read input.txt, consult calculate, write result.txt, then reply done.", Repetitions: 1, Files: map[string]string{"input.txt": "21"}, Fixtures: []harness.SkillBehaviorFixture{{Name: "calculate", Description: "Calculate a fixture result", Parameters: map[string]harness.SkillBehaviorParameter{"value": {Type: "integer", Required: true}}, Exchanges: []harness.SkillBehaviorExchange{{Arguments: json.RawMessage(`{"value":21}`), Output: "42"}}}}, Assertions: []harness.SkillBehaviorAssertion{{ID: "value", Kind: "file_equals", Path: "result.txt", Expected: "42"}, {ID: "called", Kind: "tool_calls", Tool: "calculate", Count: 1}}}}
}
func writeBehaviorAnswer(w http.ResponseWriter, content string, calls ...map[string]any) {
	message := map[string]any{"role": "assistant", "content": content}
	reason := "stop"
	if len(calls) > 0 {
		message["tool_calls"] = calls
		reason = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}})
}
func behaviorCall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func TestSkillBehaviorActualToolLoopVariantIsolationAndReceipts(t *testing.T) {
	s, _, claim, ctx := maintenanceReviewFixture(t, 20)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages            []struct{ Role, Content string }
			Tools               []any
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Tools) != 5 || request.MaxCompletionTokens != 1024 {
			t.Errorf("tool/config contract %+v", request)
		}
		if strings.Contains(request.Messages[0].Content, "oracle-private") || strings.Contains(request.Messages[1].Content, "oracle-private") {
			t.Error("oracle leaked to model")
		}
		turns := 0
		for _, m := range request.Messages {
			if m.Role == "tool" {
				turns++
			}
		}
		switch turns {
		case 0:
			writeBehaviorAnswer(w, "", behaviorCall("read", "read_file", `{"path":"input.txt"}`))
		case 1:
			if request.Messages[len(request.Messages)-1].Content != "21" {
				t.Error("fixture read not executed")
			}
			writeBehaviorAnswer(w, "", behaviorCall("compute", "calculate", `{"value":21}`))
		case 2:
			if request.Messages[len(request.Messages)-1].Content != "42" {
				t.Error("closed fixture not executed")
			}
			value := "42"
			if strings.Contains(request.Messages[0].Content, "base calculation") {
				value = "21"
			}
			writeBehaviorAnswer(w, "", behaviorCall("write", "write_file", fmt.Sprintf(`{"path":"result.txt","content":%q}`, value)))
		default:
			writeBehaviorAnswer(w, "done")
		}
	}))
	defer server.Close()
	evaluator, err := NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL, APIKey: "private"}})
	if err != nil {
		t.Fatal(err)
	}
	input := behaviorInput()
	input.Case.Assertions = append(input.Case.Assertions, harness.SkillBehaviorAssertion{ID: "oracle-private", Kind: "output_equals", Expected: "done"})
	candidate, err := evaluator.Evaluate(ctx, input)
	if err != nil || !candidate.Passed || candidate.ModelCalls != 4 || candidate.ToolCalls != 3 || candidate.InputTokens != 400 || candidate.OutputTokens != 40 || candidate.UsageEstimated {
		t.Fatalf("candidate %+v %v", candidate, err)
	}
	input.Variant = "base"
	input.Files["SKILL.md"] = []byte("Follow the task; use the base calculation.")
	base, err := evaluator.Evaluate(ctx, input)
	if err != nil || base.Passed || base.ErrorCode != "assertion_failed" || base.ModelCalls != 4 || !base.Checks[1].Passed {
		t.Fatalf("base %+v %v", base, err)
	}
	if input.Case.Files["input.txt"] != "21" || len(input.Case.Files) != 1 {
		t.Fatal("runner changed host fixtures")
	}
	receipts, err := s.Receipts(ctx, claim.Job.ID, 0, 100)
	if err != nil || len(receipts) != 8 || calls.Load() != 8 {
		t.Fatalf("receipts %d calls%d %v", len(receipts), calls.Load(), err)
	}
	for _, receipt := range receipts {
		if receipt.Stage != "behavior" || receipt.State != "settled" {
			t.Fatalf("unmetered model call %+v", receipt)
		}
	}
}

func TestSkillBehaviorClosedToolsBoundsAndOracleSeparation(t *testing.T) {
	for _, mode := range []string{"unknown", "traversal", "duplicate", "arguments", "exchange", "model-limit", "budget", "oracle", "no-skill"} {
		t.Run(mode, func(t *testing.T) {
			allowed := int64(20)
			if mode == "budget" {
				allowed = 1
			}
			_, _, _, ctx := maintenanceReviewFixture(t, allowed)
			var providerCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := providerCalls.Add(1)
				var request any
				_ = json.NewDecoder(r.Body).Decode(&request)
				encoded, _ := json.Marshal(request)
				if strings.Contains(string(encoded), "SECRET_ORACLE") {
					t.Error("oracle sent to provider")
				}
				switch mode {
				case "unknown":
					writeBehaviorAnswer(w, "", behaviorCall("x", "bash", `{"command":"echo external"}`))
				case "traversal":
					writeBehaviorAnswer(w, "", behaviorCall("x", "write_file", `{"path":"../escape.txt","content":"bad"}`))
				case "duplicate":
					writeBehaviorAnswer(w, "", behaviorCall("x", "list_files", `{}`), behaviorCall("x", "list_files", `{}`))
				case "arguments":
					writeBehaviorAnswer(w, "", behaviorCall("x", "read_file", `{"path":"input.txt","path":"other"}`))
				case "exchange":
					writeBehaviorAnswer(w, "", behaviorCall("x", "calculate", `{"value":22}`))
				case "model-limit", "budget":
					writeBehaviorAnswer(w, "", behaviorCall(fmt.Sprint(n), "list_files", `{}`))
				case "oracle", "no-skill":
					writeBehaviorAnswer(w, "done")
				}
			}))
			defer server.Close()
			evaluator, err := NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}, MaxModelCalls: 2})
			if err != nil {
				t.Fatal(err)
			}
			input := behaviorInput()
			input.Case.Assertions = []harness.SkillBehaviorAssertion{{ID: "hidden", Kind: "output_equals", Expected: "SECRET_ORACLE"}}
			if mode == "no-skill" {
				input.Version = ""
				input.Files = nil
				input.Case.Assertions[0].Expected = "done"
			}
			result, err := evaluator.Evaluate(ctx, input)
			want := map[string]string{"unknown": "unknown_tool", "traversal": "invalid_tool_arguments", "duplicate": "duplicate_tool_call", "arguments": "invalid_tool_arguments", "exchange": "fixture_mismatch", "model-limit": "model_limit", "budget": "budget_exhausted", "oracle": "assertion_failed", "no-skill": ""}[mode]
			if result.ErrorCode != want || result.Passed != (mode == "no-skill") || mode != "budget" && err != nil || mode == "budget" && err == nil {
				t.Fatalf("result %+v %v want%s", result, err, want)
			}
			if len(result.OutputDigest) != 64 || len(result.TraceDigest) != 64 || len(result.Checks) != 1 || len(result.Checks[0].ActualDigest) != 64 {
				t.Fatalf("invalid digest report %+v", result)
			}
			if mode == "budget" && providerCalls.Load() != 1 {
				t.Fatal("provider call bypassed budget")
			}
		})
	}
}

func TestSkillBehaviorWorkspaceReadonlySkillAndCleanup(t *testing.T) {
	input := behaviorInput()
	w, err := newBehaviorWorkspace(input.Files, input.Case)
	if err != nil {
		t.Fatal(err)
	}
	directory := w.directory
	output, code := w.invoke(context.Background(), "read_skill", `{"path":"SKILL.md"}`)
	if code != "" || output != string(input.Files["SKILL.md"]) {
		t.Fatal(code, output)
	}
	if _, code = w.invoke(context.Background(), "write_file", `{"path":"SKILL.md","content":"task-file"}`); code != "" {
		t.Fatal(code)
	}
	output, code = w.invoke(context.Background(), "read_skill", `{"path":"SKILL.md"}`)
	if code != "" || output != string(input.Files["SKILL.md"]) {
		t.Fatal("task write mutated pinned skill")
	}
	outside := filepath.Join(filepath.Dir(directory), "must-not-write-"+filepath.Base(directory))
	if _, code = w.invoke(context.Background(), "write_file", fmt.Sprintf(`{"path":%q,"content":"bad"}`, outside)); code != "invalid_tool_arguments" {
		t.Fatal("absolute write permitted", code)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("outside effect", err)
	}
	if err = w.close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("temporary fixture directory retained", err)
	}
	if skillbehavior.Hash(nil) == "" {
		t.Fatal("missing empty digest")
	}
}

func TestSkillBehaviorCancellationAndUnknownUsageKeepCharge(t *testing.T) {
	for _, mode := range []string{"cancelled", "missing-usage", "provider-error"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, parent := maintenanceReviewFixture(t, 4)
			started := make(chan struct{}, 1)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if mode == "cancelled" {
					started <- struct{}{}
					<-r.Context().Done()
					return
				}
				if mode == "provider-error" {
					http.Error(w, "private-provider-details", 503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()
			evaluator, err := NewSkillBehaviorEvaluator(parent, harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}})
			if err != nil {
				t.Fatal(err)
			}
			input := behaviorInput()
			input.Case.Assertions = []harness.SkillBehaviorAssertion{{ID: "done", Kind: "output_equals", Expected: "done"}}
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			type outcome struct {
				result harness.SkillBehaviorResult
				err    error
			}
			finished := make(chan outcome, 1)
			go func() { result, err := evaluator.Evaluate(ctx, input); finished <- outcome{result, err} }()
			if mode == "cancelled" {
				select {
				case <-started:
					cancel()
				case <-time.After(5 * time.Second):
					t.Fatal("behavior model request did not start")
				}
			}
			var got outcome
			select {
			case got = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("behavior model request did not terminate")
			}
			if mode == "missing-usage" && (!got.result.Passed || got.err != nil) || mode != "missing-usage" && got.err == nil || !got.result.UsageEstimated || got.result.ModelCalls != 1 || calls.Load() != 1 {
				t.Fatalf("result %+v error=%v calls=%d", got.result, got.err, calls.Load())
			}
			if mode == "cancelled" && got.result.ErrorCode != "cancelled" || mode == "provider-error" && got.result.ErrorCode != "model_error" {
				t.Fatalf("failure classification %+v", got.result)
			}
			if got.err != nil && strings.Contains(got.err.Error(), "private-provider-details") {
				t.Fatal("provider diagnostics leaked")
			}
			receipts, err := s.Receipts(parent, claim.Job.ID, 0, 100)
			if err != nil || len(receipts) != 1 || receipts[0].ChargedTokens != receipts[0].ReservedTokens || !receipts[0].Estimated {
				t.Fatalf("failed/missing usage refunded %+v %v", receipts, err)
			}
		})
	}
}

func TestSkillBehaviorSharedEvaluatorConcurrentCaseIsolation(t *testing.T) {
	_, _, _, leftContext := maintenanceReviewFixture(t, 10)
	_, _, _, rightContext := maintenanceReviewFixture(t, 10)
	var atRead atomic.Int64
	releaseReads := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
			Tools    []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		label := request.Messages[1].Content
		lookup := "lookup_" + label
		found := false
		for _, tool := range request.Tools {
			if tool.Function.Name == lookup {
				found = true
			} else if strings.HasPrefix(tool.Function.Name, "lookup_") {
				t.Errorf("peer tool contract leaked into %s: %s", label, tool.Function.Name)
			}
		}
		if !found {
			t.Error("case fixture tool missing", lookup)
		}
		turns := 0
		for _, message := range request.Messages {
			if message.Role == "tool" {
				turns++
			}
		}
		switch turns {
		case 0:
			writeBehaviorAnswer(w, "", behaviorCall("lookup", lookup, `{}`), behaviorCall("write", "write_file", fmt.Sprintf(`{"path":"result.txt","content":%q}`, label)))
		case 2:
			// Both cases have written the same relative path before either may
			// read it. Reusing a workspace would deterministically corrupt one.
			if atRead.Add(1) == 2 {
				close(releaseReads)
			}
			select {
			case <-releaseReads:
			case <-r.Context().Done():
				return
			}
			writeBehaviorAnswer(w, "", behaviorCall("read", "read_file", `{"path":"result.txt"}`))
		default:
			actual := request.Messages[len(request.Messages)-1].Content
			if actual != label {
				t.Errorf("case workspace mixed: %s received %s", label, actual)
			}
			writeBehaviorAnswer(w, actual)
		}
	}))
	defer server.Close()
	evaluator, err := NewSkillBehaviorEvaluator(context.Background(), harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result harness.SkillBehaviorResult
		err    error
	}
	finished := make(chan outcome, 2)
	contexts := []context.Context{leftContext, rightContext}
	for index, label := range []string{"left", "right"} {
		input := behaviorInput()
		input.Case.Prompt = label
		input.Case.Fixtures = []harness.SkillBehaviorFixture{{Name: "lookup_" + label, Description: "Resolve this case fixture", Parameters: map[string]harness.SkillBehaviorParameter{}, Exchanges: []harness.SkillBehaviorExchange{{Arguments: json.RawMessage(`{}`), Output: label + "-fixture"}}}}
		input.Case.Assertions = []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "output_equals", Expected: label}}
		ctx, cancel := context.WithTimeout(contexts[index], 5*time.Second)
		defer cancel()
		go func() { result, err := evaluator.Evaluate(ctx, input); finished <- outcome{result, err} }()
	}
	for range 2 {
		select {
		case got := <-finished:
			if got.err != nil || !got.result.Passed || got.result.ModelCalls != 3 || got.result.ToolCalls != 3 {
				t.Fatalf("concurrent behavior %+v %v", got.result, got.err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("concurrent behavior failed to terminate")
		}
	}
}

func TestSkillBehaviorInputAndToolCapsStopBeforeFurtherEffects(t *testing.T) {
	for _, mode := range []string{"input", "tool"} {
		t.Run(mode, func(t *testing.T) {
			s, _, claim, ctx := maintenanceReviewFixture(t, 10)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				writeBehaviorAnswer(w, "", behaviorCall(fmt.Sprint(n), "write_file", `{"path":"result.txt","content":"bounded"}`))
			}))
			defer server.Close()
			cfg := harness.SkillBehaviorModelConfig{Route: harness.ModelRoute{Model: "fixture", BaseURL: server.URL}, MaxToolCalls: 1}
			input := behaviorInput()
			if mode == "input" {
				cfg.MaxInputBytes = 1024
				input.Case.Prompt = strings.Repeat("A", 2048)
			}
			evaluator, err := NewSkillBehaviorEvaluator(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := evaluator.Evaluate(ctx, input)
			if err != nil || result.Passed {
				t.Fatalf("limit unexpectedly passed %+v %v", result, err)
			}
			wantCalls, wantTools, wantCode := int64(0), int64(0), "output_limit"
			if mode == "tool" {
				wantCalls, wantTools, wantCode = 2, 1, "tool_limit"
			}
			if calls.Load() != wantCalls || result.ModelCalls != wantCalls || result.ToolCalls != wantTools || result.ErrorCode != wantCode {
				t.Fatalf("limit bypass %+v calls=%d", result, calls.Load())
			}
			receipts, err := s.Receipts(ctx, claim.Job.ID, 0, 100)
			if err != nil || int64(len(receipts)) != wantCalls {
				t.Fatalf("limited call receipts %+v %v", receipts, err)
			}
		})
	}
}
