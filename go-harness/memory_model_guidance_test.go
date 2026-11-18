package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func TestMemoryModelFixExplicitInstructionExample(t *testing.T) {
	examples := 0
	for _, line := range strings.Split(personalMemoryInstructionV4, "\n") {
		if !strings.HasPrefix(line, `{"key":`) {
			continue
		}
		var in tools.MemoryWriteInputV3
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			t.Fatal(err)
		}
		if _, err := memory.ValidateWrite(in.Evidence, memory.WriteRequest{Action: "remember", Key: in.Key, Category: in.Category, Value: in.Value, Evidence: in.Evidence, EvidenceStart: in.EvidenceStart, EvidenceEnd: in.EvidenceEnd}, false); err != nil {
			t.Fatalf("published model example cannot pass host validation: %v", err)
		}
		examples++
	}
	if examples != 1 {
		t.Fatal("explicit typed path example is missing")
	}
}

func TestMemoryModelFixInstructionVersionAndPinnedV3(t *testing.T) {
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	cfg.memoryGlobalID = c.service.MemoryGlobalID
	req := harness.RunRequest{Session: session, RunID: "guide", InputID: "guide", RootBudgetID: "guide"}
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
	if state.MemoryWriteProtocol != "memory-tools-v5" || !strings.Contains(modern.InstructionAppend, "project.path is a typed field") {
		t.Fatal("new runs lack the typed path guide or its pinned instruction version")
	}
	state.MemoryWriteProtocol = "memory-tools-v3"
	pinned, _ := json.Marshal(state)
	legacy, err := factory(t.Context(), req, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Cleanup != nil {
		defer legacy.Cleanup()
	}
	if !bytes.Equal(legacy.State, pinned) || !strings.Contains(legacy.InstructionAppend, personalMemoryInstructionV3) || strings.Contains(legacy.InstructionAppend, "project.path is a typed field") {
		t.Fatal("resuming V3 changed its pinned instruction contract")
	}
	original, err := tools.MemoryMutationToolsV3(func(context.Context, string, tools.MemoryWriteInputV3) (any, error) { return nil, nil }, func(context.Context, tools.MemoryStateInput) (any, error) { return nil, nil })
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
		if expected, ok := want[info.Name]; ok {
			raw, _ := json.Marshal(info)
			if string(raw) != expected {
				t.Fatalf("V3 tool contract changed: %s", info.Name)
			}
			delete(want, info.Name)
		}
	}
	if len(want) != 0 {
		t.Fatal("pinned V3 lost a memory tool")
	}
}

func TestMemoryModelFixSDKTypedPathStillRejectsGenericValue(t *testing.T) {
	for _, correct := range []bool{true, false} {
		name, value := "path_only", "src/app/main.go"
		if !correct {
			name, value = "whole_fact_is_not_a_path_value", "项目路径是 src/app/main.go"
		}
		t.Run(name, func(t *testing.T) {
			text := "请记住项目路径是 src/app/main.go"
			var calls atomic.Int32
			c, session := memoryStage0Client(t, Config{MemorySingleUser: true, PermissionMode: harness.PermissionModeOff}, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					memoryStage0Completion(t, w, "", "remember_memory", map[string]any{"scope": "workspace", "key": "project.path", "category": "project", "value": value, "evidence": text, "evidence_start": 0, "evidence_end": len([]rune(text)), "expected_revision": 0})
					return
				}
				memoryStage0Completion(t, w, "done", "", nil)
			})
			if _, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: text}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			page, err := c.MemoryFacts(t.Context(), session.ID, harness.MemoryWorkspace, "", 10)
			if err != nil || len(page.Facts) != map[bool]int{true: 1, false: 0}[correct] {
				t.Fatalf("typed value boundary: facts=%d err=%v", len(page.Facts), err)
			}
			receipts, err := c.ListToolReceipts(t.Context(), session.ID)
			want := harness.ReceiptNotExecuted
			if correct {
				want = harness.ReceiptCompleted
			}
			if err != nil || len(receipts) != 1 || receipts[0].State != want {
				t.Fatalf("mutation receipt: %+v %v", receipts, err)
			}
			if !correct && !strings.Contains(receipts[0].Error, "unsupported_value_or_content") {
				t.Fatal("wrong value rejection lost its reason")
			}
		})
	}
}
