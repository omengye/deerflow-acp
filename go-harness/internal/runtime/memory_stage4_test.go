package runtime

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"testing"
	"time"
)

func TestMemoryStage4FixedFlushWaitAllowsNewForeground(t *testing.T) {
	s, native, _, session := budgetService(t, harness.BudgetLimits{}, budget.Config{})
	mem, err := memory.New(t.Context(), native.DB())
	if err != nil {
		t.Fatal(err)
	}
	s.Memory, s.MemoryAsync = mem, true
	mem.SetExtractionWorkerRunning(true)
	add := func(id, sid, state string) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := native.DB().Exec("INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'completed',?,?)", id, sid, "input-"+id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := native.DB().Exec("INSERT INTO memory_inputs(input_id,run_id,session_id,manifest,decision,job_id) VALUES(?,?,?,'{}','queued',?)", "input-"+id, id, sid, id); err != nil {
			t.Fatal(err)
		}
		if _, err := native.DB().Exec("INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,status,available_at,updated_at) VALUES(?,?,?,'test','{}',?,?,?)", id, id, "input-"+id, state, now, now); err != nil {
			t.Fatal(err)
		}
	}
	add("first", session.ID, "queued")
	other, err := s.NewSession(t.Context(), "other", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	add("other", other.ID, "blocked")
	ctx, _, release, err := s.memoryScope(t.Context(), "owner", session.ID, harness.MemorySession)
	if err != nil {
		t.Fatal(err)
	}
	set, err := mem.CaptureFlush(ctx, session.ID)
	release()
	if err != nil {
		t.Fatal(err)
	}
	observed, resume := make(chan struct{}), make(chan struct{})
	finished := make(chan harness.MemoryFlushStatus, 1)
	errs := make(chan error, 1)
	go func() {
		status, err := s.waitMemoryFlush(t.Context(), set, func(ctx context.Context) error {
			close(observed)
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		finished <- status
		errs <- err
	}()
	select {
	case <-observed:
	case <-time.After(3 * time.Second):
		t.Fatal("flush did not observe pending job")
	}
	select {
	case status := <-finished:
		t.Fatalf("premature success: %+v", status)
	default:
	}
	_, release, err = s.Coordinator.Begin(t.Context(), session.ID, "owner")
	if err != nil {
		t.Fatal("wait held foreground slot", err)
	}
	add("later", session.ID, "queued")
	release()
	if _, err = native.DB().Exec("UPDATE memory_jobs SET status='completed' WHERE id='first'"); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case status := <-finished:
		if err := <-errs; err != nil || !status.Flushed || status.Total != 1 || status.Completed != 1 {
			t.Fatalf("expanded set: %+v %v", status, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("later input or peer blocked fixed set")
	}
}
