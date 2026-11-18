package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

func TestMemoryStage1ResumeKeepsOriginalSourceVersions(t *testing.T) {
	for _, action := range []string{"correct", "forget", "correct_reopen", "forget_reopen"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			c, session, cfg := memoryStage0ConfiguredClient(t, Config{
				MemorySingleUser: true, MemoryExtraction: true, PermissionMode: harness.PermissionModeAll,
			}, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					memoryStage0Completion(t, w, "", "write_file", map[string]any{"path": "result.txt", "content": "done"})
					return
				}
				memoryStage0Completion(t, w, "done", "", nil)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			scope := memoryStage0GlobalScope(t, c)
			seed, err := c.service.Memory.Mutate(ctx, scope, memory.Mutation{
				OperationID: "stage0-seed", Action: "remember", Key: "city", Category: "profile",
				Content: "用户最近告知所在地是上海。", Source: memory.Source{ID: "seed", Kind: "operator"},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil,
				func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
					return harness.PermissionCancelled, nil
				})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
				t.Fatalf("fixture must pause a real execution: %+v %v", result, err)
			}
			var originalManifest []byte
			if err = c.store.DB().QueryRowContext(ctx, "SELECT manifest FROM memory_inputs WHERE run_id=?", result.Execution.RunID).Scan(&originalManifest); err != nil {
				t.Fatal(err)
			}
			const corrected = "用户最近告知所在地是深圳。"
			mutation := memory.Mutation{
				OperationID: "stage0-newer-action", Action: "remember", Key: "city", Category: "profile",
				Content: corrected, ExpectedRevision: seed.Revision, Source: memory.Source{ID: "newer-action", Kind: "operator"},
			}
			if strings.HasPrefix(action, "forget") {
				mutation.Action, mutation.Content = "forget", ""
			}
			if _, err = c.service.Memory.Mutate(ctx, scope, mutation); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(action, "_reopen") {
				if err = c.Close(); err != nil {
					t.Fatal(err)
				}
				c, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				reopened := c
				t.Cleanup(func() {
					if err := reopened.Close(); err != nil {
						t.Error(err)
					}
				})
				c.memoryCancel()
				select {
				case <-c.memoryDone:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if _, err = c.LoadSession(ctx, session.ID, session.CWD, false, nil); err != nil {
					t.Fatal(err)
				}
			}
			result, err = c.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{
				RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version,
			}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 2 {
				t.Fatalf("fixture resume failed: %+v calls=%d err=%v", result, calls.Load(), err)
			}
			var resumedManifest []byte
			if err = c.store.DB().QueryRowContext(ctx, "SELECT manifest FROM memory_inputs WHERE run_id=?", result.Execution.RunID).Scan(&resumedManifest); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(originalManifest, resumedManifest) {
				t.Fatal("resume rewrote accepted-source manifest")
			}
			var payload []byte
			if err = c.store.DB().QueryRowContext(ctx, "SELECT payload FROM memory_jobs WHERE run_id=?", result.Execution.RunID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var job memory.Job
			if err = json.Unmarshal(payload, &job); err != nil {
				t.Fatal(err)
			}
			raw := memoryStage2Extraction("global", "city", "profile", "北京", "我在北京", 0)
			if err = stage3ApplySDKFixture(t, c, ctx, job, raw); err != nil {
				t.Fatal(err)
			}
			fact, getErr := c.service.Memory.Get(ctx, scope, seed.FactID)
			if strings.HasPrefix(action, "correct") {
				if getErr != nil || fact.Content != corrected || fact.Revision != seed.Revision+1 {
					t.Errorf("P1-13: resumed old input overwrote a newer correction; revision=%d content=%q err=%v", fact.Revision, fact.Content, getErr)
				}
			} else if !errors.Is(getErr, harness.ErrNotFound) {
				t.Errorf("P1-13: resumed old input resurrected a deleted fact: %+v %v", fact, getErr)
			}
			// A tombstone currently hides the rebase error. Checking the source
			// version also catches the regression before F6 permits relearning.
			source, err := c.service.Memory.InputSnapshot(ctx, job.RunID, job.InputID)
			if err != nil {
				t.Fatal(err)
			}
			if job.Version != 2 {
				t.Fatalf("wrong job protocol: %d", job.Version)
			}
			if got := source.Scopes[memory.GlobalScope].Slots[memory.CanonicalKey("city")]; got != seed.Revision {
				t.Errorf("P1-13: resume rebased an old input onto a newer slot version; got=%d want=%d", got, seed.Revision)
			}
		})
	}
}

func TestMemoryStage1ForgetByIDProducesDurableReceipt(t *testing.T) {
	const prompt = "请忘记我之前保存的所在地"
	var calls atomic.Int32
	var factID string
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, PermissionMode: harness.PermissionModeOff}, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "fixture decoding error", http.StatusBadRequest)
			return
		}
		var system string
		for _, message := range request.Messages {
			if message.Role == "system" {
				system += string(message.Content)
			}
		}
		call := calls.Add(1)
		if strings.Contains(system, "用户最近告知所在地是北京。") != (call == 1) {
			t.Error("unslotted fact projection did not reflect ID deletion")
		}
		if call == 1 {
			memoryStage0Completion(t, w, "", "forget_memory", map[string]any{
				"fact_id": factID, "scope": "global", "category": "profile", "content": "",
				"evidence": prompt, "evidence_start": 0, "evidence_end": len([]rune(prompt)), "expected_revision": 1,
			})
			return
		}
		memoryStage0Completion(t, w, "done", "", nil)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	scope := memoryStage0GlobalScope(t, c)
	fact, err := c.service.Memory.Create(ctx, scope, memory.Candidate{
		Content: "用户最近告知所在地是北京。", Category: "profile", Confidence: 1, Source: memory.Source{ID: "seed", Kind: "operator"},
	})
	if err != nil {
		t.Fatal(err)
	}
	factID = fact.ID
	key, err := c.service.Memory.FactKey(ctx, scope, factID)
	if err != nil || key != "" {
		t.Fatalf("fixture must have no slot: key=%q err=%v", key, err)
	}
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 2 {
		t.Fatalf("fixture tool run failed: %+v calls=%d err=%v", result, calls.Load(), err)
	}
	if _, err = c.service.Memory.Get(ctx, scope, factID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("P1-2: forget_memory cannot delete an unslotted fact by ID; get err=%v", err)
	}
	var receipts int
	if err = c.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_operations WHERE json_extract(receipt,'$.fact_id')=? AND json_extract(receipt,'$.status')='forgotten'", factID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("P1-2: ID deletion did not atomically persist its receipt; receipts=%d", receipts)
	}
}

func memoryStage0GlobalScope(t *testing.T, c *Client) memory.Scope {
	t.Helper()
	scope, err := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestMemoryStage1AcceptedSourceFailureRollsBackAdmission(t *testing.T) {
	var calls atomic.Int32
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		memoryStage0Completion(t, w, "done", "", nil)
	})
	if _, err := c.store.DB().Exec(`CREATE TRIGGER fail_source BEFORE INSERT ON memory_inputs BEGIN SELECT RAISE(ABORT,'source_failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "source_failure") {
		t.Fatalf("wrong admission failure: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("model ran without an accepted source")
	}
	for _, table := range []string{"harness_runs", "harness_inputs", "harness_events", "memory_inputs", "budget_roots"} {
		var count int
		if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s escaped acceptance rollback: %d %v", table, count, err)
		}
	}
}

func TestMemoryStage1MissingLegacySourceSkipsWithoutBlockingResume(t *testing.T) {
	for _, personal := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "personal"}[personal], func(t *testing.T) {
			var calls atomic.Int32
			c, session := memoryStage0Client(t, Config{MemorySingleUser: personal, MemoryExtraction: true, PermissionMode: harness.PermissionModeAll}, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					memoryStage0Completion(t, w, "", "write_file", map[string]any{"path": "result.txt", "content": "done"})
					return
				}
				memoryStage0Completion(t, w, "done", "", nil)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.PermissionCancelled, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
				t.Fatalf("pause: %+v %v", result, err)
			}
			if _, err = c.store.DB().Exec("DELETE FROM memory_inputs WHERE run_id=?", result.Execution.RunID); err != nil {
				t.Fatal(err)
			}
			result, err = c.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			})
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 2 {
				t.Fatalf("resume: %+v calls=%d err=%v", result, calls.Load(), err)
			}
			var jobs, manifests, skipped int
			if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_jobs").Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_inputs").Scan(&manifests); err != nil {
				t.Fatal(err)
			}
			if err = c.store.DB().QueryRow("SELECT COUNT(*) FROM memory_extraction_audit WHERE status='legacy_source_snapshot_missing'").Scan(&skipped); err != nil {
				t.Fatal(err)
			}
			if jobs != 0 || manifests != 0 || skipped != 1 {
				t.Fatalf("legacy source rebased or silently lost: jobs=%d manifests=%d skipped=%d", jobs, manifests, skipped)
			}
		})
	}
}

func TestMemoryStage1LegacyCheckpointKeepsToolAndResourceContract(t *testing.T) {
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	cfg.memoryGlobalID = c.service.MemoryGlobalID
	req := harness.RunRequest{Session: session, RunID: "legacy-contract", InputID: "legacy-input", RootBudgetID: "legacy-contract"}
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
	if state.MemoryWriteProtocol != personalMemoryWriteProtocol {
		t.Fatal("new input lacks versioned tools")
	}
	state.MemoryWriteProtocol = ""
	pinned, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := factory(t.Context(), req, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Cleanup != nil {
		defer legacy.Cleanup()
	}
	if !bytes.Equal(legacy.State, pinned) {
		t.Fatal("legacy execution state was silently upgraded")
	}
	original, err := tools.MemoryMutationTools(func(context.Context, string, tools.MemoryWriteInput) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for _, candidate := range original {
		info, err := candidate.Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(info)
		expected[info.Name] = string(raw)
	}
	found := 0
	for _, candidate := range legacy.Tools {
		info, err := candidate.Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "get_memory_state" {
			t.Fatal("new tool changed a legacy checkpoint contract")
		}
		if old, ok := expected[info.Name]; ok {
			found++
			raw, _ := json.Marshal(info)
			if string(raw) != old {
				t.Fatalf("legacy schema changed for %s", info.Name)
			}
			_, err = candidate.(tool.InvokableTool).InvokableRun(t.Context(), `{"key":"city","scope":"global","content":"","category":"profile","evidence":"请忘记城市","expected_revision":0}`)
			if !errors.Is(err, memory.ErrSourceSnapshotMissing) {
				t.Fatalf("legacy tool performed a new write: %v", err)
			}
		}
	}
	if found != 2 {
		t.Fatalf("legacy mutation tools missing: %d", found)
	}
}

func TestMemoryStage1RecordsDecisionWithoutJob(t *testing.T) {
	for _, decision := range []string{"disabled", "no_facts", "quota_skip", "promoted"} {
		t.Run(decision, func(t *testing.T) {
			cfg := Config{MemoryExtraction: decision != "disabled"}
			if decision == "quota_skip" {
				cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 256}
			}
			c, session := memoryStage0Client(t, cfg, func(w http.ResponseWriter, r *http.Request) {
				if decision == "promoted" {
					memoryStage0ExtractionReply(t, w, r)
					return
				}
				memoryStage0Completion(t, w, "{\"schema_version\":2,\"facts\":[]}", "", nil)
			})
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "From now on, please answer concisely in Chinese"}}, nil, nil)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("run: %+v %v", result, err)
			}
			var got, job string
			if err = c.store.DB().QueryRow("SELECT decision,job_id FROM memory_inputs WHERE run_id=?", result.Execution.RunID).Scan(&got, &job); err != nil {
				t.Fatal(err)
			}
			if got != decision || job != "" {
				t.Fatalf("no-job outcome is unexplained: decision=%s job=%s", got, job)
			}
		})
	}
}

func TestMemoryStage1ConcurrentSessionsPinEitherSideOfClear(t *testing.T) {
	accepted, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	defer releaseOld()
	var calls atomic.Int32
	c, first := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(accepted)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		memoryStage0Completion(t, w, "done", "", nil)
	})
	second, err := c.NewSession(t.Context(), first.CWD)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type outcome struct {
		result harness.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := c.Run(ctx, first.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
		done <- outcome{result, err}
	}()
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	scope := memoryStage0GlobalScope(t, c)
	if _, err = c.service.Memory.Clear(ctx, scope, 0, "clear", "operator"); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.Run(ctx, second.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil || fresh.Execution == nil || fresh.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("new source: %+v %v", fresh, err)
	}
	releaseOld()
	var old outcome
	select {
	case old = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if old.err != nil || old.result.Execution == nil || old.result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("old source: %+v %v", old.result, old.err)
	}
	for i, result := range []harness.RunResult{old.result, fresh} {
		var raw []byte
		if err = c.store.DB().QueryRow("SELECT payload FROM memory_jobs WHERE run_id=?", result.Execution.RunID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var job memory.Job
		if err = json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		source, err := c.service.Memory.InputSnapshot(ctx, job.RunID, job.InputID)
		if err != nil || source.Scopes[memory.GlobalScope].DeleteEpoch != int64(i) {
			t.Fatalf("acceptance did not order with Clear: %+v %v", source, err)
		}
		if err = stage3ApplySDKFixture(t, c, ctx, job, memoryStage2Extraction("global", "city", "profile", "北京", "我在北京", 0)); err != nil {
			t.Fatal(err)
		}
		page, err := c.service.Memory.List(ctx, scope, "", 10)
		if err != nil || len(page.Facts) != i {
			t.Fatalf("deletion epoch did not fence only the old input: count=%d err=%v", len(page.Facts), err)
		}
	}
}

func TestMemoryStage1StaleExplicitRestoreHasNoUncertainEffect(t *testing.T) {
	const prompt = "请记住我在北京"
	var calls atomic.Int32
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, PermissionMode: harness.PermissionModeAll}, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			memoryStage0Completion(t, w, "", "write_file", map[string]any{"path": "result.txt", "content": "done"})
		case 2:
			memoryStage0Completion(t, w, "", "remember_memory", map[string]any{
				"scope": "global", "key": "city", "category": "profile", "expected_revision": 2,
				"evidence": prompt, "evidence_start": 0, "evidence_end": len([]rune(prompt)), "value": "北京", "content": "用户最近告知所在地是北京。",
			})
		default:
			memoryStage0Completion(t, w, "done", "", nil)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	scope := memoryStage0GlobalScope(t, c)
	seed, err := c.service.Memory.Mutate(ctx, scope, memory.Mutation{OperationID: "seed", Action: "remember", Key: "city", Category: "profile", Content: "北京", Source: memory.Source{ID: "seed", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.PermissionCancelled, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatalf("pause: %+v %v", result, err)
	}
	if _, err = c.service.Memory.Mutate(ctx, scope, memory.Mutation{OperationID: "delete", Action: "forget", Key: "city", ExpectedRevision: seed.Revision, Source: memory.Source{ID: "delete", Kind: "operator"}}); err != nil {
		t.Fatal(err)
	}
	result, err = c.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: result.Execution.RunID, ExpectedVersion: result.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 3 {
		t.Fatalf("safe refusal blocked main task: %+v calls=%d err=%v", result, calls.Load(), err)
	}
	var state string
	if err = c.store.DB().QueryRow("SELECT state FROM harness_tool_receipts WHERE run_id=? AND tool_call_id='stage0-remember_memory'", result.Execution.RunID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(harness.ReceiptNotExecuted) {
		t.Fatalf("policy refusal became an uncertain mutation: %s", state)
	}
	if _, err = c.service.Memory.Get(ctx, scope, seed.FactID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("stale explicit input resurrected deletion: %v", err)
	}
}
