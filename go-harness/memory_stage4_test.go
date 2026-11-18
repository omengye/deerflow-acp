package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func stage4QueuedClient(t *testing.T) (*Client, harness.Session) {
	t.Helper()
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	if _, err := c.Run(t.Context(), session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	return c, session
}

func TestMemoryStage4SDKStatusAndCompatibility(t *testing.T) {
	c, session := stage4QueuedClient(t)
	status, err := c.FlushMemoryWithStatus(t.Context(), session.ID)
	if err != nil || status.Flushed || status.Pending != 1 || status.Reason != "worker_not_running" {
		t.Fatalf("%+v %v", status, err)
	}
	if err = c.FlushMemory(t.Context(), session.ID); !errors.Is(err, harness.ErrMemoryFlushPending) {
		t.Fatalf("compatibility: %v", err)
	}
	for _, tc := range []struct {
		state, code string
		sentinel    error
	}{{"blocked", "budget_unknown", harness.ErrMemoryFlushBlocked}, {"failed", "invalid_envelope", harness.ErrMemoryFlushFailed}} {
		if _, err = c.store.DB().Exec("UPDATE memory_jobs SET status=?,error_code=?", tc.state, tc.code); err != nil {
			t.Fatal(err)
		}
		var typed *harness.MemoryFlushError
		err = c.FlushMemory(t.Context(), session.ID)
		if !errors.Is(err, tc.sentinel) || !errors.As(err, &typed) || typed.Status.Flushed {
			t.Fatalf("%+v %v", typed, err)
		}
	}
	if _, err = c.store.DB().Exec("UPDATE memory_jobs SET status='completed',error_code='quota_skip'"); err != nil {
		t.Fatal(err)
	}
	status, err = c.FlushMemoryWithStatus(t.Context(), session.ID)
	if err != nil || !status.Flushed || status.Skipped != 1 || status.Completed != 0 {
		t.Fatalf("quota: %+v %v", status, err)
	}
	if _, err = c.service.FlushMemoryWithStatus(t.Context(), "foreign-owner", session.ID); err == nil {
		t.Fatal("foreign owner flushed")
	}
}

func TestMemoryStage4FlushTimeoutCancellationAndReleasedSlot(t *testing.T) {
	c, session := stage4QueuedClient(t)
	c.service.Memory.SetExtractionWorkerRunning(true)
	defer c.service.Memory.SetExtractionWorkerRunning(false)
	short, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	status, err := c.FlushMemoryWithStatus(short, session.ID)
	if err != nil || status.Flushed || status.Pending != 1 || status.Reason != "timeout" {
		t.Fatalf("timeout: %+v %v", status, err)
	}
	if !errors.Is(status.Err(), context.DeadlineExceeded) {
		t.Fatal("timeout compatibility")
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, err = c.FlushMemoryWithStatus(cancelled, session.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	// The first call has observed a pending set. A second foreground admission
	// remains possible, and cannot be held hostage to background processing.
	_, release, err := c.service.Coordinator.Begin(t.Context(), session.ID, c.owner)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestMemoryStage4FlushTotalTenSecondBound(t *testing.T) {
	c, session := stage4QueuedClient(t)
	c.service.Memory.SetExtractionWorkerRunning(true)
	defer c.service.Memory.SetExtractionWorkerRunning(false)
	start := time.Now()
	status, err := c.FlushMemoryWithStatus(t.Context(), session.ID)
	elapsed := time.Since(start)
	if err != nil || status.Reason != "timeout" || status.Pending != 1 || elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("bound: %+v %v %s", status, err, elapsed)
	}
}

func TestMemoryStage4FlushWaitsForPaidResultPublication(t *testing.T) {
	c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	grant := stage3Grant(t, c, j)
	if first, err := c.service.Memory.DispatchJobCall(t.Context(), j, grant); err != nil || !first {
		t.Fatalf("dispatch: %v", err)
	}
	stage3Response(t, c, j)
	if err := c.service.Memory.SettleJobResponse(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	c.service.Memory.SetExtractionWorkerRunning(true)
	defer c.service.Memory.SetExtractionWorkerRunning(false)
	// A settled response is still pending until its facts/receipts commit.
	probe, stop := context.WithTimeout(t.Context(), 60*time.Millisecond)
	before, err := c.FlushMemoryWithStatus(probe, j.SessionID)
	stop()
	if err != nil || before.Pending != 1 || before.Flushed {
		t.Fatalf("before: %+v %v", before, err)
	}
	if err = c.service.Memory.ApplyJob(t.Context(), j, j.Result); err == nil {
		t.Fatal("empty caller result accepted")
	}
	// ApplyJob accepts the durable raw response, without another model call.
	if err = c.service.Memory.ApplyJob(t.Context(), j, stage3Raw(t)); err != nil {
		t.Fatal(err)
	}
	after, err := c.FlushMemoryWithStatus(t.Context(), j.SessionID)
	if err != nil || !after.Flushed || after.Completed != 1 || after.Barrier != before.Barrier {
		t.Fatalf("after: %+v %v", after, err)
	}
	page, err := c.MemoryFacts(t.Context(), j.SessionID, harness.MemoryGlobal, "", 10)
	if err != nil || len(page.Facts) != 1 {
		t.Fatalf("facts: %+v %v", page, err)
	}
}

func TestMemoryStage4SessionDeletionDisposesJobsAtomically(t *testing.T) {
	c, session := stage4QueuedClient(t)
	if _, err := c.store.DB().Exec("CREATE TRIGGER stage4_delete_failure BEFORE DELETE ON harness_sessions BEGIN SELECT RAISE(ABORT,'delete_failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.DeleteAttachedSession(t.Context(), c.owner, session.ID); err == nil {
		t.Fatal("fault ignored")
	}
	var state string
	if err := c.store.DB().QueryRow("SELECT status FROM memory_jobs").Scan(&state); err != nil || state != "queued" {
		t.Fatalf("half deletion: %s %v", state, err)
	}
	if _, err := c.store.DB().Exec("DROP TRIGGER stage4_delete_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.DeleteSession(t.Context(), session.ID); err != nil {
		t.Fatal(err)
	}
	var reason, usage string
	if err := c.store.DB().QueryRow("SELECT j.status,d.reason,d.usage_state FROM memory_jobs j JOIN memory_job_dispositions d ON d.job_id=j.id").Scan(&state, &reason, &usage); err != nil || state != "discarded" || reason != "session_deleted" || usage != "no_active_call" {
		t.Fatalf("%s %s %s %v", state, reason, usage, err)
	}
	if err := c.service.Memory.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStage4DeletionPreservesUnknownBudget(t *testing.T) {
	c, _, j := stage3Client(t, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	grant := stage3Grant(t, c, j)
	if _, err := c.service.Memory.DispatchJobCall(t.Context(), j, grant); err != nil {
		t.Fatal(err)
	}
	if err := c.service.Memory.AbortJob(t.Context(), j, context.Canceled); err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.DeleteAttachedSession(t.Context(), c.owner, j.SessionID); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("unknown deleted: %v", err)
	}
	var held int
	if err := c.store.DB().QueryRow("SELECT held_tokens FROM budget_roots WHERE id=?", j.RootBudgetID).Scan(&held); err != nil || held == 0 {
		t.Fatalf("unknown released: %d %v", held, err)
	}
}

func TestMemoryStage4IndexWorkerIndependentAndDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			var calls atomic.Int32
			called := make(chan struct{}, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				called <- struct{}{}
				var req struct {
					Input []string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				vectors := make([][]float64, len(req.Input))
				for i := range vectors {
					vectors[i] = []float64{1, 0}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "test", "vectors": vectors})
			}))
			defer server.Close()
			c, err := Open(t.Context(), Config{DataDir: t.TempDir(), MemoryEnabled: &enabled, MemorySingleUser: true, MemoryExtraction: false, MemoryEmbeddingURL: server.URL, MemoryEmbeddingModel: "test", Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
				return harness.RunResult{}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			session, err := c.NewSession(t.Context(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.CreateMemoryFact(t.Context(), session.ID, harness.MemoryGlobal, harness.MemoryCandidate{Content: "我在北京", Category: "profile", Confidence: 1}); err != nil {
				t.Fatal(err)
			}
			if enabled {
				done := c.memoryIndexDone
				c.startMemoryIndexWorker()
				if c.memoryIndexDone != done {
					t.Fatal("duplicate index worker")
				}
				select {
				case <-called:
				case <-time.After(5 * time.Second):
					t.Fatal("extraction disabled blocked indexing")
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					var indexed int
					if err = c.store.DB().QueryRow("SELECT count(*) FROM memory_vectors WHERE model='test'").Scan(&indexed); err != nil {
						t.Fatal(err)
					}
					if indexed == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("index response never committed")
					}
					timer := time.NewTimer(10 * time.Millisecond)
					<-timer.C
				}
				if c.memoryDone != nil {
					t.Fatal("extraction worker unexpectedly started")
				}
			} else {
				if c.memoryIndexDone != nil {
					t.Fatal("disabled memory started index worker")
				}
				if err = c.service.Memory.IndexPending(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err = c.SearchMemory(t.Context(), session.ID, harness.MemoryGlobal, "北京", 10); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatal("disabled memory sent embeddings")
				}
			}
		})
	}
}

func TestMemoryStage4IndexCloseJoinsProvider(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer server.Close()
	defer server.CloseClientConnections()
	c, err := Open(t.Context(), Config{DataDir: t.TempDir(), MemoryEmbeddingURL: server.URL, MemoryEmbeddingModel: "test", Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateMemoryFact(t.Context(), session.ID, harness.MemorySession, harness.MemoryCandidate{Content: "fixture", Category: "profile", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("index did not start")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("provider not cancelled")
	}
	select {
	case <-c.memoryIndexDone:
	default:
		t.Fatal("worker not joined")
	}
}
