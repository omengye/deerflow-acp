package deerflow

import (
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
	"net/http"
	"testing"
)

func TestMemoryReviewB1SkippedPostRun(t *testing.T) {
	for _, kind := range []string{"max_tokens", "empty_end_turn", "refusal", "content_filter"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{MemorySingleUser: true, MemoryExtraction: true, ToolPolicy: harness.ToolPolicy{Allowlist: []string{}}}
			if kind == "max_tokens" {
				cfg.Budget = &harness.BudgetLimits{MaxTokens: 64, MaxModelCalls: 2, MaxOutputTokens: 16}
			}
			c, session, cfg := memoryStage0ConfiguredClient(t, cfg, func(w http.ResponseWriter, r *http.Request) {
				if kind == "refusal" || kind == "content_filter" {
					if err := modelstreamtest.WriteCompletion(w, map[string]any{
						"id": "review-refusal", "object": "chat.completion",
						"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Cannot answer this request."}, "finish_reason": kind}},
						"usage":   map[string]int{"prompt_tokens": 16, "completion_tokens": 8, "total_tokens": 24},
					}); err != nil {
						t.Error(err)
					}
					return
				}
				memoryStage0Completion(t, w, "", "", nil)
			})
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var decision, jobID, runStatus, stop string
			err = c.store.DB().QueryRow("SELECT i.decision,i.job_id,r.status,r.stop_reason FROM memory_inputs i JOIN harness_runs r ON r.id=i.run_id WHERE i.run_id=?", result.Execution.RunID).Scan(&decision, &jobID, &runStatus, &stop)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("run=%s stop=%s decision=%s job=%q", runStatus, stop, decision, jobID)
			expectedStop := kind
			if kind == "empty_end_turn" {
				expectedStop = "end_turn"
			}
			if kind == "content_filter" {
				expectedStop = "refusal"
			}
			if stop != expectedStop {
				t.Fatalf("fixture reached %s, want %s", stop, expectedStop)
			}
			if runStatus != "completed" || decision != "postrun_skipped" || jobID != "" {
				t.Fatalf("unexpected input/run state")
			}
			for n := 0; n < 2; n++ {
				status, e := c.FlushMemoryWithStatus(t.Context(), session.ID)
				if e != nil || !status.Flushed || status.Skipped != 1 {
					t.Fatalf("flush=%+v err=%v", status, e)
				}
			}
			if err = c.FlushMemory(t.Context(), session.ID); err != nil {
				t.Fatal(err)
			}
			// Simulate the old binary's pending row, then exercise real startup
			// reconciliation, including an idempotent second reopen below.
			if _, err = c.store.DB().Exec("UPDATE memory_inputs SET decision='pending' WHERE run_id=?", result.Execution.RunID); err != nil {
				t.Fatal(err)
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c2, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c2.Close()
			if _, err = c2.LoadSession(t.Context(), session.ID, session.CWD, false, nil); err != nil {
				t.Fatal(err)
			}
			status, err := c2.FlushMemoryWithStatus(t.Context(), session.ID)
			if err != nil || !status.Flushed || status.Skipped != 1 {
				t.Fatalf("reopen=%+v %v", status, err)
			}
			if _, err = c2.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "继续"}}, nil, nil); err != nil {
				t.Fatal(err)
			}
			status, err = c2.FlushMemoryWithStatus(t.Context(), session.ID)
			if err != nil || !status.Flushed || status.Skipped != 2 {
				t.Fatalf("later turn=%+v %v", status, err)
			}
		})
	}
}
func TestMemoryReviewB6aOptionalSourceError(t *testing.T) {
	for _, fault := range []string{"missing_scope", "invalid_scope", "missing_provenance"} {
		t.Run(fault, func(t *testing.T) {
			c, session := memoryStage0Client(t, Config{MemoryExtraction: true, Budget: &harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 256}}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var input string
			if err = c.store.DB().QueryRow("SELECT input_id FROM harness_runs WHERE id=?", result.Execution.RunID).Scan(&input); err != nil {
				t.Fatal(err)
			}
			raw := `{"schema_version":2,"facts":[{"scope":"workspace","key":"profile.last_reported_location","category":"profile","value":"北京","evidence":"我在北京","evidence_start":0,"evidence_end":4,"durability":"durable","authority":"descriptive","confidence":0.9}]}`
			if err = c.service.Memory.SaveExtractionResult(t.Context(), result.Execution.RunID, input, raw, memory.UserSupportedProvenance); err != nil {
				t.Fatal(err)
			}
			// Fault injection only: remove the pinned scope from the synthetic input.
			if _, err = c.store.DB().Exec("UPDATE memory_inputs SET decision='pending'"); err != nil {
				t.Fatal(err)
			}
			req := harness.RunRequest{RunID: result.Execution.RunID, InputID: input, Session: session}
			switch fault {
			case "missing_scope":
				_, err = c.store.DB().Exec("UPDATE memory_inputs SET manifest=json_set(CAST(manifest AS TEXT),'$.scopes',json('{}'))")
			case "invalid_scope":
				req.Session.CWD = ""
			case "missing_provenance":
				_, err = c.store.DB().Exec("ALTER TABLE memory_extraction_results RENAME TO saved_results; CREATE VIEW memory_extraction_results AS SELECT id,run_id,input_id,raw FROM saved_results")
			}
			if err != nil {
				t.Fatal(err)
			}
			post, err := memoryPostRunFactory(c.service.Memory, req, "", harness.MemoryWorkspace, nil)(t.Context(), memoryStage2ErrorModel{errors.New("unexpected model call")})
			if err != nil {
				t.Fatal(err)
			}
			ctx := budget.WithScope(t.Context(), budget.Scope{MemberID: req.RunID, SessionID: session.ID, AttemptID: "probe"})
			got := post(ctx, "done", true)
			if got != nil {
				t.Fatalf("missing scope returned %v", got)
			}
			var decision string
			if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs WHERE input_id=?", input).Scan(&decision); err != nil || decision != "source_unavailable" {
				t.Fatalf("decision=%s err=%v", decision, err)
			}
		})
	}
}

func TestMemoryReviewB1HistoricalRepairRequiresEvidence(t *testing.T) {
	for _, kind := range []string{"started", "model_error", "audit", "result", "normal_answer", "bad_event", "bad_digest", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			answer := ""
			if kind == "normal_answer" {
				answer = "done"
			}
			c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true, ToolPolicy: harness.ToolPolicy{Allowlist: []string{}}}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, answer, "", nil) })
			result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			run := result.Execution.RunID
			var input string
			if err = c.store.DB().QueryRow("SELECT input_id FROM memory_inputs WHERE run_id=?", run).Scan(&input); err != nil {
				t.Fatal(err)
			}
			if _, err = c.store.DB().Exec("DELETE FROM memory_job_leases; DELETE FROM memory_jobs; UPDATE memory_inputs SET decision='pending',job_id=''"); err != nil {
				t.Fatal(err)
			}
			want := "pending"
			switch kind {
			case "started", "model_error":
				want = kind
				_, err = c.store.DB().Exec("UPDATE memory_inputs SET decision=?", kind)
			case "audit":
				err = c.service.Memory.AuditExtraction(t.Context(), run, input, "review", "started", "", nil)
				if err == nil {
					_, err = c.store.DB().Exec("UPDATE memory_inputs SET decision='pending'")
				}
			case "result":
				err = c.service.Memory.SaveExtractionResult(t.Context(), run, input, `{"schema_version":2,"facts":[]}`, memory.UserSupportedProvenance)
			case "bad_event":
				_, err = c.store.DB().Exec("UPDATE harness_events SET event='{' WHERE run_id=?", run)
			case "bad_digest":
				_, err = c.store.DB().Exec("UPDATE memory_inputs SET manifest=json_set(CAST(manifest AS TEXT),'$.input_sha','bad')")
			case "unknown":
				_, err = c.store.DB().Exec("UPDATE budget_roots SET blocked_reason='unknown'")
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err = c.service.Memory.RecoverJobs(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := c.service.Memory.InputDecision(t.Context(), run, input)
			if err != nil || got != want {
				t.Fatalf("history changed: %s want %s %v", got, want, err)
			}
		})
	}
}
