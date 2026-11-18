package deerflow

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func TestMemoryStage5SDKRelearningUsesPaidWorkerAndFlush(t *testing.T) {
	var extraction atomic.Value
	extraction.Store("")
	var calls atomic.Int32
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		text := extraction.Load().(string)
		if text == "" {
			text = "done"
		}
		memoryStage0Completion(t, w, text, "", nil)
	})
	scope := memoryStage0GlobalScope(t, c)
	var factID string
	for i, value := range []string{"北京", "北京", "上海"} {
		text := "我在" + value
		result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: text}}, nil, nil)
		if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
			t.Fatalf("run %d: %v", i, err)
		}
		j, err := c.service.Memory.ClaimJob(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		extraction.Store(memoryStage2Extraction("global", "city", "profile", value, text, 0))
		err = c.processMemoryJob(t.Context(), stage3Extractor(t, cfg), j)
		extraction.Store("")
		if err != nil {
			t.Fatal(err)
		}
		snap, err := c.budgets.Snapshot(t.Context(), result.Execution.RunID)
		if err != nil || snap.ModelCalls != 2 || snap.SpentTokens != 48 || snap.HeldTokens != 0 {
			t.Fatalf("unmetered relearning: %+v %v", snap, err)
		}
		state, err := c.service.Memory.KeyState(t.Context(), scope, "city")
		if err != nil || state.Deleted != (i == 1) || state.Revision != int64(i+1) {
			t.Fatalf("state %d: %+v %v", i, state, err)
		}
		if i == 0 {
			page, err := c.service.Memory.List(t.Context(), scope, "", 10)
			if err != nil || len(page.Facts) != 1 {
				t.Fatalf("seed: %+v %v", page, err)
			}
			factID = page.Facts[0].ID
			if err = c.service.Memory.Delete(t.Context(), scope, factID, state.Revision, memory.Source{ID: "sdk-delete", Kind: "operator"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, err := c.FlushMemoryWithStatus(t.Context(), session.ID)
	if err != nil || !status.Flushed || status.Completed != 2 || status.Skipped != 1 || status.Total != 3 {
		t.Fatalf("flush: %+v %v", status, err)
	}
	page, err := c.service.Memory.List(t.Context(), scope, "", 10)
	if err != nil || len(page.Facts) != 1 || page.Facts[0].ID != factID || page.Facts[0].Content != "用户最近告知所在地是上海。" || calls.Load() != 6 {
		t.Fatalf("published fact/call count: %+v calls=%d %v", page, calls.Load(), err)
	}
	if err = c.FlushMemory(t.Context(), session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStage5SDKExplicitToolPreservesRestoreAuthority(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name, text := "statement", "我在北京"
		if explicit {
			name, text = "request", "请记住我在北京"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			c, session := memoryStage0Client(t, Config{MemorySingleUser: true, PermissionMode: harness.PermissionModeOff}, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					memoryStage0Completion(t, w, "", "remember_memory", map[string]any{"scope": "global", "key": "city", "category": "profile", "value": "北京", "evidence": text, "evidence_start": 0, "evidence_end": len([]rune(text)), "expected_revision": 2})
					return
				}
				memoryStage0Completion(t, w, "done", "", nil)
			})
			scope := memoryStage0GlobalScope(t, c)
			seed, err := c.service.Memory.Mutate(t.Context(), scope, memory.Mutation{OperationID: "seed", Action: "remember", Key: "city", Category: "profile", Content: "用户最近告知所在地是北京。", Source: memory.Source{ID: "seed", Kind: "operator"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = c.service.Memory.Delete(t.Context(), scope, seed.FactID, seed.Revision, memory.Source{ID: "delete", Kind: "operator"}); err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: text}}, nil, nil)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("tool run: %v", err)
			}
			state, err := c.service.Memory.KeyState(t.Context(), scope, "city")
			wantRevision := int64(2)
			if explicit {
				wantRevision++
			}
			if err != nil || state.Deleted == explicit || state.Revision != wantRevision || calls.Load() != 2 {
				t.Fatalf("tool authority: %+v calls=%d %v", state, calls.Load(), err)
			}
			var status string
			if err = c.store.DB().QueryRow("SELECT json_extract(CAST(o.receipt AS TEXT),'$.status') FROM memory_operations o JOIN memory_operation_sources s ON s.operation_id=o.operation_id WHERE s.run_id=?", result.Execution.RunID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := "requires_explicit"
			if explicit {
				want = "updated"
			}
			if status != want {
				t.Fatalf("wrong tool receipt: %s", status)
			}
		})
	}
}
