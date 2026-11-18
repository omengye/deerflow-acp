package deerflow

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Stage-0 reproduction promoted to the default regression suite in stage 4.

func TestMemoryFixReproFlushDoesNotSucceedWithPendingJob(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemorySingleUser: true, MemoryExtraction: true},
		func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("fixture run failed: %+v %v", result, err)
	}
	var jobs int
	if err = c.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_jobs WHERE run_id=? AND status='queued'", result.Execution.RunID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("fixture requires one pending job: jobs=%d err=%v", jobs, err)
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer flushCancel()
	if err = c.FlushMemory(flushCtx, session.ID); err == nil {
		t.Fatal("P1-3: FlushMemory reported success while its session still had a pending job")
	}
}
