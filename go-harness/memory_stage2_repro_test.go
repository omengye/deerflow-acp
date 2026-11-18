package deerflow

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryStage2QuotaSkipDoesNotEnqueue(t *testing.T) {
	var calls atomic.Int32
	c, session := memoryStage0Client(t, Config{
		MemorySingleUser: true, MemoryExtraction: true,
		Budget: &harness.BudgetLimits{MaxModelCalls: 1, MaxOutputTokens: 256},
	}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		memoryStage0Completion(t, w, "done", "", nil)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "我在北京"}}, nil, nil)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted || calls.Load() != 1 {
		t.Fatalf("fixture must complete using its sole model allowance: %+v calls=%d err=%v", result, calls.Load(), err)
	}
	var jobs int
	if err = c.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_jobs WHERE run_id=?", result.Execution.RunID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("P1-4: exhausted foreground model allowance still enqueued extraction; jobs=%d", jobs)
	}
}

func TestMemoryStage2OptionalAuditFailureDoesNotFailAnswer(t *testing.T) {
	c, session := memoryStage0Client(t, Config{MemoryExtraction: true}, func(w http.ResponseWriter, r *http.Request) {
		memoryStage0ExtractionReply(t, w, r)
	})
	if _, err := c.store.DB().Exec("CREATE TRIGGER stage0_optional_audit_failure BEFORE INSERT ON memory_extraction_audit BEGIN SELECT RAISE(ABORT,'stage0_optional_audit_failure'); END"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "From now on, please answer concisely in Chinese"}}, nil, nil)
	if err != nil {
		if !strings.Contains(err.Error(), "stage0_optional_audit_failure") {
			t.Fatalf("unexpected fixture failure: %v", err)
		}
		t.Fatalf("P2-9: an optional extraction audit insert failed an otherwise healthy answer: %v", err)
	}
	if result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("P2-9: optional audit failure prevented completion: %+v", result)
	}
}
