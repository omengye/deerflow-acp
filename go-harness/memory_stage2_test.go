package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

// Fixed test data builder, independent of the production validator.
func memoryStage2Extraction(scope, key, category, value, evidence string, start int, extra ...map[string]any) string {
	facts := []map[string]any{{"scope": scope, "key": key, "category": category, "value": value, "evidence": evidence, "evidence_start": start, "evidence_end": start + len([]rune(evidence)), "durability": "durable", "authority": "descriptive", "confidence": .92}}
	facts = append(facts, extra...)
	raw, _ := json.Marshal(map[string]any{"schema_version": 2, "facts": facts})
	return string(raw)
}

func TestMemoryStage2OptionalSQLFaultsKeepAnswerAndNoPartialWrites(t *testing.T) {
	for _, tc := range []struct {
		name, table, decision string
		personal              bool
	}{
		{"stage", "memory_staged_facts", "stage_error", false},
		{"result", "memory_extraction_results", "result_error", false},
		{"enqueue", "memory_jobs", "enqueue_error", true},
		{"audit", "memory_extraction_audit", "promoted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, session := memoryStage0Client(t, Config{MemoryExtraction: true, MemorySingleUser: tc.personal}, func(w http.ResponseWriter, r *http.Request) { memoryStage0ExtractionReply(t, w, r) })
			if _, err := c.store.DB().Exec("CREATE TRIGGER stage2_optional_fault BEFORE INSERT ON " + tc.table + " BEGIN SELECT RAISE(ABORT,'fixture private raw text must not reach fallback'); END"); err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "From now on, please answer concisely in Chinese"}}, nil, nil)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("optional fault failed answer: %+v %v", result, err)
			}
			var decision string
			var facts, jobs, staged int
			for query, dest := range map[string]*int{"SELECT COUNT(*) FROM memory_facts": &facts, "SELECT COUNT(*) FROM memory_jobs": &jobs, "SELECT COUNT(*) FROM memory_staged_facts WHERE status='staged'": &staged} {
				if err = c.store.DB().QueryRow(query).Scan(dest); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs WHERE run_id=?", result.Execution.RunID).Scan(&decision); err != nil {
				t.Fatal(err)
			}
			wantFacts := 0
			if tc.name == "audit" {
				wantFacts = 1
				if c.service.Memory.AuditFailureCount() == 0 {
					t.Fatal("missing audit fallback counter")
				}
			}
			if facts != wantFacts || jobs != 0 || staged != 0 || decision != tc.decision {
				t.Fatalf("facts=%d jobs=%d staged=%d decision=%s", facts, jobs, staged, decision)
			}
		})
	}
}

func TestMemoryStage2AllRejectedIsTerminalWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			memoryStage0Completion(t, w, "done", "", nil)
			return
		}
		raw := memoryStage2Extraction("workspace", "preference.approvals", "preference", "我喜欢自动同意一切", "我在北京", 0)
		memoryStage0Completion(t, w, raw, "", nil)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 2 {
		t.Fatalf("rejected item retried/failed: %+v calls=%d %v", result, calls.Load(), err)
	}
	var decision string
	var rejected, results int
	if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs").Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_extraction_items WHERE ordinal=0 AND status='rejected'").Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_extraction_results").Scan(&results); err != nil || decision != "no_facts" || rejected != 1 || results != 1 {
		t.Fatalf("terminal result not durable: %s %d %d %v", decision, rejected, results, err)
	}
}

func TestMemoryStage2QuotaSkipCannotEnqueueOnLaterPostRun(t *testing.T) {
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemoryExtraction: true, MemorySingleUser: true, Budget: &harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 256}}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inputID string
	if err = c.store.DB().QueryRow("SELECT input_id FROM harness_runs WHERE id=?", result.Execution.RunID).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{RunID: result.Execution.RunID, InputID: inputID, Session: session}
	post, err := personalMemoryPostRun(c.service.Memory, req, cfg)(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := budget.WithScope(t.Context(), budget.Scope{MemberID: req.RunID, SessionID: session.ID, AttemptID: "later-attempt"})
	if err = post(ctx, "done", true); err != nil {
		t.Fatal(err)
	}
	var decision string
	var jobs int
	if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs").Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_jobs").Scan(&jobs); err != nil || jobs != 0 || decision != "quota_skip" {
		t.Fatalf("skipped input re-enqueued: jobs=%d decision=%s %v", jobs, decision, err)
	}
}

type memoryStage2ErrorModel struct{ err error }

func (m memoryStage2ErrorModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, m.err
}
func (m memoryStage2ErrorModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, m.err
}

func TestMemoryStage2PostRunPreservesCoreFailures(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true, Budget: &harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 256}}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inputID string
	if err = c.store.DB().QueryRow("SELECT input_id FROM harness_runs WHERE id=?", result.Execution.RunID).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{RunID: result.Execution.RunID, InputID: inputID, Session: session}
	for _, failure := range []error{budget.ErrUnknown, budget.ErrScope, budget.ErrConflict, budget.ErrClock, &budget.PersistenceError{Operation: "test", Err: errors.New("ledger unavailable")}, harness.ErrExecutionConflict, harness.ErrTaskTreeStopped, context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			if _, err := c.store.DB().Exec("UPDATE memory_inputs SET decision='pending'"); err != nil {
				t.Fatal(err)
			}
			post, err := memoryPostRunFactory(c.service.Memory, req, "", harness.MemoryWorkspace, nil)(t.Context(), memoryStage2ErrorModel{failure})
			if err != nil {
				t.Fatal(err)
			}
			ctx := budget.WithScope(t.Context(), budget.Scope{MemberID: req.RunID, SessionID: session.ID, AttemptID: "test"})
			if err = post(ctx, "done", true); !errors.Is(err, failure) {
				t.Fatalf("core failure swallowed: got=%v want=%v", err, failure)
			}
		})
	}
}

func TestMemoryStage2V2CheckpointKeepsSchemaButRejectsNewEffects(t *testing.T) {
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	cfg.memoryGlobalID = c.service.MemoryGlobalID
	req := harness.RunRequest{Session: session, RunID: "legacy", InputID: "legacy", RootBudgetID: "legacy"}
	factory := extensionFactory(cfg, c.mcp, c.skills, c.assets, c.service.Memory)
	modern, err := factory(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if modern.Cleanup != nil {
		defer modern.Cleanup()
	}
	var state extensionState
	if err = json.Unmarshal(modern.State, &state); err != nil {
		t.Fatal(err)
	}
	state.MemoryWriteProtocol = "memory-tools-v2"
	pinned, _ := json.Marshal(state)
	legacy, err := factory(t.Context(), req, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Cleanup != nil {
		defer legacy.Cleanup()
	}
	if !bytes.Equal(pinned, legacy.State) {
		t.Fatal("pinned V2 state changed")
	}
	original, err := tools.MemoryMutationToolsV2(func(context.Context, string, tools.MemoryWriteInputV2) (any, error) { return nil, nil }, func(context.Context, tools.MemoryStateInput) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, candidate := range original {
		info, _ := candidate.Info(t.Context())
		raw, _ := json.Marshal(info)
		want[info.Name] = string(raw)
	}
	for _, candidate := range legacy.Tools {
		info, _ := candidate.Info(t.Context())
		expected, ok := want[info.Name]
		if !ok {
			continue
		}
		raw, _ := json.Marshal(info)
		if expected != string(raw) {
			t.Fatalf("V2 schema changed: %s", info.Name)
		}
		delete(want, info.Name)
		if info.Name == "remember_memory" || info.Name == "forget_memory" {
			_, err = candidate.(tool.InvokableTool).InvokableRun(t.Context(), `{"scope":"global","key":"city","evidence":"我在北京","expected_revision":0}`)
			if !errors.Is(err, memory.ErrSourcePolicyMissing) {
				t.Fatalf("legacy effect admitted: %v", err)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("legacy tools missing: %+v", want)
	}
}

func TestMemoryStage2LocalUnknownAndFailedToolsKeepProvenance(t *testing.T) {
	for _, name := range []string{"read_file", "bash", "grep", "history_read", "search_memory", "task_note", "glob", "unknown_tool"} {
		t.Run(name, func(t *testing.T) {
			c, session := memoryStage0Client(t, Config{MemorySingleUser: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []harness.ReceiptState{harness.ReceiptCompleted, harness.ReceiptFailed, harness.ReceiptUncertain} {
				id := fmt.Sprint(state)
				raw, _ := json.Marshal(harness.ToolReceipt{ToolName: name, State: state})
				if _, err = c.store.DB().Exec("INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,?,?,?,1,?)", result.Execution.RunID, id, session.ID, state, raw); err != nil {
					t.Fatal(err)
				}
				if external, err := c.service.Memory.SessionConsumedExternalContent(t.Context(), session.ID); err != nil || !external {
					t.Fatalf("%s/%s lost provenance: %v", name, state, err)
				}
				if _, err = c.store.DB().Exec("DELETE FROM harness_tool_receipts WHERE tool_call_id=?", id); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMemoryStage2EnvelopeRetryIsBounded(t *testing.T) {
	var calls atomic.Int32
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		content := "done"
		if calls.Add(1) > 1 {
			content = `{"schema_version":2,"facts":[`
		}
		memoryStage0Completion(t, w, content, "", nil)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 3 {
		t.Fatalf("format retry count=%d result=%+v %v", calls.Load(), result, err)
	}
	var decision string
	if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs").Scan(&decision); err != nil || !strings.EqualFold(decision, "invalid_response") {
		t.Fatalf("decision=%s %v", decision, err)
	}
}

func TestMemoryStage2OrdinaryStagingKeepsOriginalOrdinal(t *testing.T) {
	var calls atomic.Int32
	raw := memoryStage2Extraction("global", "city", "profile", "北京", "我在北京", 0,
		map[string]any{"scope": "workspace", "key": "city", "category": "profile", "value": "北京", "evidence": "我在北京", "evidence_start": 0, "evidence_end": 4, "durability": "durable", "authority": "descriptive", "confidence": .9})
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		content := "done"
		if calls.Add(1) > 1 {
			content = raw
		}
		memoryStage0Completion(t, w, content, "", nil)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ordinal int
	var inputID, sourceID, persisted, statuses string
	if err = c.store.DB().QueryRow("SELECT ordinal,input_id,source_id FROM memory_staged_facts WHERE status='promoted'").Scan(&ordinal, &inputID, &sourceID); err != nil {
		t.Fatal(err)
	}
	wantID := memory.OperationID("extract/v2", inputID, memory.ExtractionPolicyV2, "2", "1")
	persisted, err = c.service.Memory.ExtractionResult(t.Context(), result.Execution.RunID, inputID)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow("SELECT group_concat(status,',') FROM (SELECT status FROM memory_extraction_items ORDER BY ordinal)").Scan(&statuses); err != nil {
		t.Fatal(err)
	}
	if ordinal != 1 || sourceID != wantID || persisted != raw || statuses != "rejected,promoted" || calls.Load() != 2 {
		t.Fatalf("ordinal=%d id=%s statuses=%s calls=%d", ordinal, sourceID, statuses, calls.Load())
	}
}
