package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func TestMemoryStage3ClaimHasDedicatedBudgetAttempt(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		memoryStage0Completion(t, w, "done", "", nil)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := c.service.Memory.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = c.store.DB().QueryRow("SELECT count(*) FROM budget_attempts a JOIN budget_members m ON m.id=a.member_id WHERE m.kind='memory_job' AND m.root_id=? AND m.session_id=? AND a.state='active' AND a.id<>?", result.Execution.RunID, session.ID, j.AttemptID).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("memory job must have its own live, source-root budget attempt; count=%d err=%v", count, err)
	}
}

func stage3Client(t *testing.T, handler http.HandlerFunc) (*Client, Config, memory.Job) {
	t.Helper()
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true, MemoryExtraction: true}, handler)
	if _, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	j, err := c.service.Memory.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg, j
}
func stage3Extractor(t *testing.T, cfg Config) *einoengine.MemoryExtractor {
	t.Helper()
	e, err := einoengine.NewMemoryExtractor(t.Context(), einoengine.Config{API: cfg.API, Provider: cfg.Provider, APIKey: cfg.APIKey, BaseURL: cfg.BaseURL, Model: cfg.Model})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func stage3Raw(t *testing.T) string {
	return memoryStage2Extraction("global", "city", "profile", "北京", "我在北京", 0)
}
func stage3Grant(t *testing.T, c *Client, j memory.Job) budget.Reservation {
	t.Helper()
	g, err := c.service.Memory.ReserveJobCall(t.Context(), j, "fixture", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func stage3Response(t *testing.T, c *Client, j memory.Job) {
	t.Helper()
	if err := c.service.Memory.SaveJobResponse(t.Context(), j, memory.JobResponse{Raw: stage3Raw(t), Complete: true, Usage: harness.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}); err != nil {
		t.Fatal(err)
	}
}
func stage3State(t *testing.T, c *Client, j memory.Job) (string, string, int) {
	t.Helper()
	var status, result string
	var n int
	if err := c.store.DB().QueryRow("SELECT status,result FROM memory_jobs WHERE id=?", j.ID).Scan(&status, &result); err != nil {
		t.Fatal(err)
	}
	if err := c.store.DB().QueryRow("SELECT count(*) FROM memory_facts WHERE deleted=0").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return status, result, n
}
func TestMemoryStage3WorkerChargesUsageAndCapsOutput(t *testing.T) {
	var calls atomic.Int32
	c, cfg, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			memoryStage0Completion(t, w, "done", "", nil)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["max_tokens"] != float64(1024) && body["max_completion_tokens"] != float64(1024) {
			t.Errorf("output cap: %v", body["max_tokens"])
		}
		memoryStage0Completion(t, w, stage3Raw(t), "", nil)
	})
	if err := c.processMemoryJob(t.Context(), stage3Extractor(t, cfg), j); err != nil {
		t.Fatal(err)
	}
	snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	status, _, facts := stage3State(t, c, j)
	var active, owners int
	_ = c.store.DB().QueryRow("SELECT count(*) FROM budget_attempts WHERE member_id=? AND state='active'", j.MemberID).Scan(&active)
	_ = c.store.DB().QueryRow("SELECT count(*) FROM budget_operation_owners WHERE operation_id=? AND root_id=?", j.OperationID, j.RootBudgetID).Scan(&owners)
	if calls.Load() != 2 || snap.ModelCalls != 2 || snap.SpentTokens != 48 || snap.HeldTokens != 0 || status != "completed" || facts != 1 || active != 0 || owners != 1 {
		t.Fatalf("calls=%d snap=%+v status=%s facts=%d active=%d owners=%d", calls.Load(), snap, status, facts, active, owners)
	}
}

func TestMemoryStage3RecoveryWindows(t *testing.T) {
	for _, point := range []string{"claimed", "reserved", "dispatched", "response", "settled", "applied"} {
		t.Run(point, func(t *testing.T) {
			c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			if point != "claimed" {
				g := stage3Grant(t, c, j)
				if point != "reserved" {
					if first, err := c.service.Memory.DispatchJobCall(t.Context(), j, g); err != nil || !first {
						t.Fatalf("dispatch=%v %v", first, err)
					}
					if point != "dispatched" {
						stage3Response(t, c, j)
					}
					if point == "settled" || point == "applied" {
						if err := c.service.Memory.SettleJobResponse(t.Context(), j); err != nil {
							t.Fatal(err)
						}
					}
					if point == "applied" {
						if err := c.service.Memory.ApplyJob(t.Context(), j, stage3Raw(t)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if err := c.service.Memory.RecoverJobs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.service.Memory.RecoverJobs(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, raw, facts := stage3State(t, c, j)
			snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
			if err != nil {
				t.Fatal(err)
			}
			if point == "dispatched" {
				if status != "blocked" || snap.BlockedReason != "unknown" || snap.HeldTokens != 110 || facts != 0 {
					t.Fatalf("%s %+v facts=%d", status, snap, facts)
				}
				if next, err := c.service.Memory.ClaimJob(t.Context()); next.ID != "" || !errors.Is(err, harness.ErrNotFound) {
					t.Fatalf("unknown redispatched: %+v %v", next, err)
				}
				return
			}
			if snap.HeldTokens != 0 || snap.BlockedReason != "" {
				t.Fatalf("recovery leaked hold: %+v", snap)
			}
			if point == "response" || point == "settled" {
				if status != "queued" || raw != stage3Raw(t) || snap.SpentTokens != 39 {
					t.Fatalf("status=%s raw=%s snap=%+v", status, raw, snap)
				}
				_ = c.service.Memory.SetDailyAttempts(0)
				next, err := c.service.Memory.ClaimJob(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if next.Result != raw {
					t.Fatal("lost saved response")
				}
				if err = c.service.Memory.ApplyJob(t.Context(), next, raw); err != nil {
					t.Fatal(err)
				}
				after, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
				if err != nil || after.ModelCalls != snap.ModelCalls || after.SpentTokens != snap.SpentTokens {
					t.Fatalf("replay charged twice: %+v %v", after, err)
				}
			} else if point == "applied" {
				if status != "completed" || facts != 1 {
					t.Fatalf("status=%s facts=%d", status, facts)
				}
			} else if status != "queued" || facts != 0 {
				t.Fatalf("status=%s facts=%d", status, facts)
			}
		})
	}
}

func TestMemoryStage3ScopeAndDispatchFences(t *testing.T) {
	for _, alter := range []string{"root", "member", "session", "attempt", "lease", "cancelled", "explicit", "delete", "quota"} {
		t.Run(alter, func(t *testing.T) {
			c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			forged := j
			switch alter {
			case "root":
				forged.RootBudgetID = "another"
			case "member":
				forged.MemberID = j.RunID
			case "session":
				forged.SessionID = "another"
			case "attempt":
				forged.WorkerAttemptID = j.AttemptID
			case "lease":
				_, _ = c.store.DB().Exec("UPDATE memory_job_leases SET lease_ns=0 WHERE job_id=?", j.ID)
			case "cancelled":
				_, _ = c.store.DB().Exec("UPDATE memory_jobs SET status='discarded' WHERE id=?", j.ID)
			case "explicit":
				scope, err := memory.NewScope(memory.GlobalScope, "", "", j.ProfileID, "")
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.service.Memory.Mutate(t.Context(), scope, memory.Mutation{SchemaVersion: 2, OperationID: "explicit", Action: "forget", Key: "city", Source: memory.Source{Kind: "operator", ID: "explicit", RunID: j.RunID, InputID: j.InputID, PolicyVersion: memory.ExplicitPolicyV2}})
				if err != nil {
					t.Fatal(err)
				}
			case "delete":
				m, err := c.service.Memory.InputSnapshot(t.Context(), j.RunID, j.InputID)
				if err != nil {
					t.Fatal(err)
				}
				for _, scope := range m.Scopes {
					_, err = c.store.DB().Exec("INSERT INTO memory_scope_barriers(scope_key,epoch) VALUES(?,1) ON CONFLICT(scope_key) DO UPDATE SET epoch=epoch+1", scope.Key)
					if err != nil {
						t.Fatal(err)
					}
				}
			case "quota":
				_, err := c.store.DB().Exec("UPDATE budget_roots SET blocked_reason='tokens' WHERE id=?", j.RootBudgetID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.service.Memory.ReserveJobCall(t.Context(), forged, "fixture", 10, 100); err == nil {
				t.Fatal("forged/stale admission accepted")
			}
		})
	}
	t.Run("change_after_reserve", func(t *testing.T) {
		c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
		g := stage3Grant(t, c, j)
		_, _ = c.store.DB().Exec("UPDATE memory_jobs SET status='discarded' WHERE id=?", j.ID)
		if first, err := c.service.Memory.DispatchJobCall(t.Context(), j, g); err == nil || first {
			t.Fatalf("cancelled dispatch: %v %v", first, err)
		}
	})
}

func TestMemoryStage3ResponseSettlementAndApplyFaults(t *testing.T) {
	for _, fault := range []string{"settlement", "publication"} {
		t.Run(fault, func(t *testing.T) {
			c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			g := stage3Grant(t, c, j)
			if _, err := c.service.Memory.DispatchJobCall(t.Context(), j, g); err != nil {
				t.Fatal(err)
			}
			stage3Response(t, c, j)
			trigger := "CREATE TRIGGER stage3_fail BEFORE UPDATE ON budget_reservations WHEN NEW.state='settled' BEGIN SELECT RAISE(ABORT,'injected'); END"
			if fault == "publication" {
				trigger = "CREATE TRIGGER stage3_fail BEFORE UPDATE ON memory_jobs WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'injected'); END"
			}
			if _, err := c.store.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			err := c.service.Memory.SettleJobResponse(t.Context(), j)
			if fault == "publication" {
				if err != nil {
					t.Fatal(err)
				}
				err = c.service.Memory.ApplyJob(t.Context(), j, stage3Raw(t))
			}
			if err == nil {
				t.Fatal("injected fault did not fire")
			}
			if _, _, facts := stage3State(t, c, j); facts != 0 {
				t.Fatal("partial fact commit")
			}
			if _, err = c.store.DB().Exec("DROP TRIGGER stage3_fail"); err != nil {
				t.Fatal(err)
			}
			if err = c.service.Memory.RecoverJobs(t.Context()); err != nil {
				t.Fatal(err)
			}
			next, err := c.service.Memory.ClaimJob(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err = c.service.Memory.ApplyJob(t.Context(), next, next.Result); err != nil {
				t.Fatal(err)
			}
			snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
			if err != nil || snap.ModelCalls != 2 || snap.SpentTokens != 39 || snap.HeldTokens != 0 {
				t.Fatalf("bad recovery: %+v %v", snap, err)
			}
		})
	}
}

func TestMemoryStage3LostResponseKeepsUnknownHold(t *testing.T) {
	var calls atomic.Int32
	c, cfg, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) {
		text := "done"
		if calls.Add(1) > 1 {
			text = stage3Raw(t)
		}
		memoryStage0Completion(t, w, text, "", nil)
	})
	if _, err := c.store.DB().Exec("CREATE TRIGGER fail_response BEFORE UPDATE ON memory_job_leases WHEN NEW.response_saved=1 BEGIN SELECT RAISE(ABORT,'response_unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if err := c.processMemoryJob(t.Context(), stage3Extractor(t, cfg), j); err == nil {
		t.Fatal("lost response not reported")
	}
	snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	status, raw, facts := stage3State(t, c, j)
	if status != "blocked" || raw != "" || facts != 0 || snap.HeldTokens <= 0 || snap.BlockedReason != "unknown" || calls.Load() != 2 {
		t.Fatalf("state=%s raw=%s facts=%d budget=%+v calls=%d", status, raw, facts, snap, calls.Load())
	}
	if _, err = c.store.DB().Exec("DROP TRIGGER fail_response"); err != nil {
		t.Fatal(err)
	}
	if err = c.service.Memory.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.service.Memory.ClaimJob(t.Context()); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("lost result retried: %v", err)
	}
}

func TestMemoryStage3FormatRetriesEachCostARequest(t *testing.T) {
	var calls atomic.Int32
	c, cfg, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) {
		content := "done"
		if calls.Add(1) > 1 {
			content = "{"
		}
		memoryStage0Completion(t, w, content, "", nil)
	})
	extractor := stage3Extractor(t, cfg)
	for n := 0; n < 3; n++ {
		if err := c.processMemoryJob(t.Context(), extractor, j); err != nil {
			t.Fatal(err)
		}
		if n < 2 {
			if _, err := c.store.DB().Exec("UPDATE memory_jobs SET available_at='2000-01-01' WHERE id=?", j.ID); err != nil {
				t.Fatal(err)
			}
			next, err := c.service.Memory.ClaimJob(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			j = next
		}
	}
	snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	status, _, facts := stage3State(t, c, j)
	if calls.Load() != 4 || snap.ModelCalls != 4 || snap.SpentTokens != 96 || snap.HeldTokens != 0 || status != "failed" || facts != 0 {
		t.Fatalf("calls=%d status=%s facts=%d snap=%+v", calls.Load(), status, facts, snap)
	}
}

// Keep compile-time checks on imported helpers used by subsequent lifecycle
// cases; the cases below exercise real host recovery rather than a raw provider.
func stage3ApplySDKFixture(t *testing.T, c *Client, ctx context.Context, j memory.Job, raw string) error {
	t.Helper()
	// Give the desired input a deterministic turn without rebasing its source.
	if _, err := c.store.DB().Exec("UPDATE memory_jobs SET available_at=CASE WHEN id=? THEN '1999-01-01' ELSE '2000-01-01' END WHERE status='queued'", j.ID); err != nil {
		return err
	}
	claimed, err := c.service.Memory.ClaimJob(ctx)
	if errors.Is(err, harness.ErrNotFound) {
		var status, reason string
		if e := c.store.DB().QueryRow("SELECT status,error_code FROM memory_jobs WHERE id=?", j.ID).Scan(&status, &reason); e != nil {
			return e
		}
		if status == "discarded" && reason == "superseded" {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if claimed.ID != j.ID {
		return budget.ErrConflict
	}
	grant := stage3Grant(t, c, claimed)
	if _, err = c.service.Memory.DispatchJobCall(ctx, claimed, grant); err != nil {
		return err
	}
	if err = c.service.Memory.SaveJobResponse(ctx, claimed, memory.JobResponse{Raw: raw, Complete: true, Usage: harness.Usage{TotalTokens: 15}}); err != nil {
		return err
	}
	if err = c.service.Memory.SettleJobResponse(ctx, claimed); err != nil {
		return err
	}
	return c.service.Memory.ApplyJob(ctx, claimed, raw)
}

func TestMemoryStage3MissingUsageAndTransportFailureStayUnknown(t *testing.T) {
	for _, failure := range []string{"missing_usage", "http_error", "broken_stream"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			c, cfg, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					memoryStage0Completion(t, w, "done", "", nil)
					return
				}
				if failure == "http_error" {
					http.Error(w, "unavailable", 503)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if failure == "broken_stream" {
					_, _ = w.Write([]byte("data: {broken}\n\n"))
					return
				}
				raw, _ := json.Marshal(map[string]any{"id": "x", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": stage3Raw(t)}, "finish_reason": "stop"}}})
				_, _ = w.Write(append(append([]byte("data: "), raw...), []byte("\n\ndata: [DONE]\n\n")...))
			})
			if err := c.processMemoryJob(t.Context(), stage3Extractor(t, cfg), j); !errors.Is(err, budget.ErrUnknown) {
				t.Fatalf("unknown result: %v", err)
			}
			snap, err := c.budgets.Snapshot(t.Context(), j.RootBudgetID)
			if err != nil {
				t.Fatal(err)
			}
			status, _, facts := stage3State(t, c, j)
			var run string
			if err = c.store.DB().QueryRow("SELECT status FROM harness_runs WHERE id=?", j.RunID).Scan(&run); err != nil {
				t.Fatal(err)
			}
			if snap.ModelCalls != 2 || snap.HeldTokens <= 0 || snap.BlockedReason != "unknown" || status != "blocked" || facts != 0 || run != "completed" || calls.Load() != 2 {
				t.Fatalf("snap=%+v status=%s facts=%d run=%s calls=%d", snap, status, facts, run, calls.Load())
			}
			if err = c.service.Memory.RecoverJobs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = c.service.Memory.ClaimJob(t.Context()); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("unknown retried: %v", err)
			}
		})
	}
}

func TestMemoryStage3RestartSettlesSavedUsageBeforeGenericRecovery(t *testing.T) {
	c, cfg, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	grant := stage3Grant(t, c, j)
	if _, err := c.service.Memory.DispatchJobCall(t.Context(), j, grant); err != nil {
		t.Fatal(err)
	}
	stage3Response(t, c, j)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MemoryExtraction = false
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next, err := reopened.service.Memory.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if next.Result != stage3Raw(t) {
		t.Fatal("saved result lost at host restart")
	}
	if err = reopened.service.Memory.ApplyJob(t.Context(), next, next.Result); err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.budgets.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil || snap.BlockedReason != "" || snap.HeldTokens != 0 || snap.ModelCalls != 2 || snap.SpentTokens != 39 {
		t.Fatalf("restart: %+v %v", snap, err)
	}
}

func TestMemoryStage3OldClaimCannotWriteAfterRecovery(t *testing.T) {
	c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	if err := c.service.Memory.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	next, err := c.service.Memory.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if next.Fence <= j.Fence {
		t.Fatal("fence was not advanced")
	}
	if err = c.service.Memory.HeartbeatJob(t.Context(), j); err == nil {
		t.Fatal("stale heartbeat accepted")
	}
	if err = c.service.Memory.SaveJobResponse(t.Context(), j, memory.JobResponse{Raw: stage3Raw(t), Complete: true}); err == nil {
		t.Fatal("stale response accepted")
	}
	if err = c.service.Memory.ApplyJob(t.Context(), j, stage3Raw(t)); err == nil {
		t.Fatal("stale publication accepted")
	}
}

func TestMemoryStage3SharedLastAdmission(t *testing.T) {
	for _, resource := range []string{"model_calls", "tokens"} {
		t.Run(resource, func(t *testing.T) {
			c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
			limits := harness.BudgetLimits{MaxModelCalls: 2, MaxOutputTokens: 100}
			if resource == "tokens" {
				limits.MaxModelCalls = 0
				limits.MaxTokens = 125
			}
			raw, _ := json.Marshal(limits)
			if _, err := c.store.DB().Exec("UPDATE budget_roots SET limits_json=? WHERE id=?", raw, j.RootBudgetID); err != nil {
				t.Fatal(err)
			}
			ledger, err := budget.New(c.store.DB(), budget.Config{CheckEffectTx: func(ctx context.Context, tx *sql.Tx, s budget.Scope) error {
				if s.MemberID == j.MemberID {
					return c.service.Memory.CheckJobBudgetEffectTx(ctx, tx, s)
				}
				if s.MemberID != "competing-main" {
					return budget.ErrScope
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err = ledger.BindMember(t.Context(), budget.MemberBinding{RootBudgetID: j.RootBudgetID, MemberID: "competing-main", SessionID: j.SessionID, OriginRunID: j.RunID, Kind: "run"}); err != nil {
				t.Fatal(err)
			}
			main := budget.Scope{RootBudgetID: j.RootBudgetID, MemberID: "competing-main", SessionID: j.SessionID, AttemptID: "competing-attempt", Fence: 1}
			if err = ledger.BeginAttempt(t.Context(), main); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			out := make(chan error, 2)
			go func() {
				<-start
				_, e := ledger.ReserveModel(t.Context(), main, budget.ModelRequest{OperationID: "competing-request", Digest: "main", InputTokens: 10, MaxOutputTokens: 100})
				out <- e
			}()
			go func() { <-start; _, e := c.service.Memory.ReserveJobCall(t.Context(), j, "memory", 10, 100); out <- e }()
			close(start)
			success := 0
			for range 2 {
				e := <-out
				if e == nil {
					success++
				} else {
					var limit *budget.LimitError
					if !errors.As(e, &limit) {
						t.Fatal(e)
					}
				}
			}
			snap, err := ledger.Snapshot(t.Context(), j.RootBudgetID)
			if err != nil || success != 1 || snap.ModelCalls != 2 || snap.HeldTokens <= 0 || snap.SpentTokens != 24 {
				t.Fatalf("success=%d snap=%+v %v", success, snap, err)
			}
		})
	}
}

func TestMemoryStage3CloseCancelsAndJoinsWorker(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			memoryStage0Completion(t, w, "done", "", nil)
			return
		}
		close(entered)
		<-r.Context().Done()
		close(exited)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.startMemoryWorker(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not enter provider")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not join worker")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("provider I/O not cancelled")
	}
	cfg.MemoryExtraction = false
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snap, err := reopened.budgets.Snapshot(t.Context(), result.Execution.RunID)
	if err != nil || snap.BlockedReason != "unknown" || snap.HeldTokens <= 0 || snap.ModelCalls != 2 || calls.Load() != 2 {
		t.Fatalf("cancel/restart lost unknown: %+v %v calls=%d", snap, err, calls.Load())
	}
}

func TestMemoryStage3ExhaustedRootCannotClaimNewCall(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true, Budget: &harness.BudgetLimits{MaxModelCalls: 2, MaxOutputTokens: 256}}, func(w http.ResponseWriter, r *http.Request) {
		memoryStage0Completion(t, w, "done", "", nil)
	})
	result, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent authorized member has consumed the last admission since enqueue.
	if _, err = c.store.DB().Exec("UPDATE budget_roots SET model_calls=2 WHERE id=?", result.Execution.RunID); err != nil {
		t.Fatal(err)
	}
	j, _ := c.service.Memory.ClaimJob(t.Context())
	var decision string
	if err = c.store.DB().QueryRow("SELECT decision FROM memory_inputs WHERE run_id=?", result.Execution.RunID).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if j.ID != "" || decision != "quota_skip" {
		t.Fatalf("exhausted root admitted worker: job=%s decision=%s", j.ID, decision)
	}
}
