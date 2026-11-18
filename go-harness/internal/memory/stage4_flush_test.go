package memory

import (
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
	"time"
)

func TestMemoryStage4FlushClassification(t *testing.T) {
	for _, tc := range []struct{ name, job, code, run, decision, want string }{
		{"queued", "queued", "", "completed", "queued", "pending"},
		{"daily", "queued", "daily_limit", "completed", "queued", "pending"},
		{"held", "queued", "budget_held", "completed", "queued", "pending"},
		{"unknown", "blocked", "budget_unknown", "completed", "budget_unknown", "blocked"},
		{"waiting", "queued", "", "waiting_input", "queued", "blocked"},
		{"failed_source", "queued", "", "failed", "queued", "discarded"},
		{"cancelled_source", "queued", "", "cancelled", "queued", "discarded"},
		{"historical_failure", "failed", "invalid_envelope", "completed", "failed", "failed"},
		{"quota_job", "completed", "quota_skip", "completed", "quota_skip", "skipped"},
		{"no_facts", "completed", "", "completed", "no_facts", "skipped"},
		{"published", "completed", "", "completed", "processed", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scope := personalStore(t)
			j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			if _, err := s.db.Exec("UPDATE memory_jobs SET status=?,error_code=?", tc.job, tc.code); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE harness_runs SET status=?", tc.run); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE memory_inputs SET decision=?", tc.decision); err != nil {
				t.Fatal(err)
			}
			set, err := s.CaptureFlush(t.Context(), j.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			status, err := s.ObserveFlush(t.Context(), set)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{"pending": status.Pending, "blocked": status.Blocked, "failed": status.Failed, "discarded": status.Discarded, "skipped": status.Skipped, "completed": status.Completed}
			if counts[tc.want] != 1 || status.Total != 1 || status.Pending+status.Blocked+status.Failed+status.Discarded+status.Skipped+status.Completed != 1 {
				t.Fatalf("%+v", status)
			}
			if tc.want == "discarded" {
				var state string
				if err = s.db.QueryRow("SELECT status FROM memory_jobs WHERE id=?", j.ID).Scan(&state); err != nil || state != "discarded" {
					t.Fatalf("durable disposition: %s %v", state, err)
				}
			}
		})
	}
}

func TestMemoryStage4FlushFixedSetAndMissingRows(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "first", "first-input", "我在北京")
	set, err := s.CaptureFlush(t.Context(), j.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	later := stage1AcceptedJob(t, s, scope, "later", "later-input", "我在上海")
	if _, err = s.db.Exec("UPDATE memory_inputs SET session_id=? WHERE input_id=?", j.SessionID, later.InputID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE memory_jobs SET status='completed' WHERE id=?", j.ID); err != nil {
		t.Fatal(err)
	}
	status, err := s.ObserveFlush(t.Context(), set)
	if err != nil || status.Total != 1 || status.Completed != 1 {
		t.Fatalf("set expanded: %+v %v", status, err)
	}
	if _, err = s.db.Exec("DELETE FROM memory_job_leases WHERE job_id=?; DELETE FROM memory_jobs WHERE id=?", j.ID, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ObserveFlush(t.Context(), set); !errors.Is(err, harness.ErrMemoryFlushState) {
		t.Fatalf("missing row silently completed: %v", err)
	}
}

func TestMemoryStage4FlushNoJobDecisions(t *testing.T) {
	for _, decision := range []string{"quota_skip", "disabled", "explicit_action", "enqueue_error", "pending"} {
		t.Run(decision, func(t *testing.T) {
			s, scope := personalStore(t)
			j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			if _, err := s.db.Exec("DELETE FROM memory_job_leases; DELETE FROM memory_jobs; UPDATE memory_inputs SET job_id='',decision=?", decision); err != nil {
				t.Fatal(err)
			}
			set, err := s.CaptureFlush(t.Context(), j.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			status, err := s.ObserveFlush(t.Context(), set)
			if err != nil {
				t.Fatal(err)
			}
			if decision == "enqueue_error" || decision == "pending" {
				if status.Failed != 1 {
					t.Fatalf("%+v", status)
				}
			} else if status.Skipped != 1 {
				t.Fatalf("%+v", status)
			}
		})
	}
}

func TestMemoryStage4FlushReportsRetryTime(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	next := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec("UPDATE memory_jobs SET available_at=?,error_code='invalid_envelope'", next); err != nil {
		t.Fatal(err)
	}
	set, err := s.CaptureFlush(t.Context(), j.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.ObserveFlush(t.Context(), set)
	if err != nil || status.Pending != 1 || status.Reason != "retry_delay" || status.NextAt != next {
		t.Fatalf("%+v %v", status, err)
	}
}

func TestMemoryStage4FlushStateErrorsAndOrphan(t *testing.T) {
	for _, fault := range []string{"missing_input", "query_failure", "orphan_run", "orphan_session"} {
		t.Run(fault, func(t *testing.T) {
			s, scope := personalStore(t)
			j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			set, err := s.CaptureFlush(t.Context(), j.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			statements := map[string]string{"missing_input": "DELETE FROM memory_inputs", "query_failure": "DROP TABLE memory_inputs", "orphan_run": "DELETE FROM harness_runs", "orphan_session": "DELETE FROM harness_sessions"}
			if _, err = s.db.Exec(statements[fault]); err != nil {
				t.Fatal(err)
			}
			status, err := s.ObserveFlush(t.Context(), set)
			if fault == "missing_input" || fault == "query_failure" {
				if !errors.Is(err, harness.ErrMemoryFlushState) || status.Flushed {
					t.Fatalf("%+v %v", status, err)
				}
			} else if err != nil || status.Blocked != 1 || status.Reason != "orphan_source" {
				t.Fatalf("%+v %v", status, err)
			}
		})
	}
}

func TestMemoryStage4RecoveryDiscardsFailedSourcesWithoutWorker(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	if _, err := s.db.Exec("UPDATE harness_runs SET status='cancelled'"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := s.db.QueryRow("SELECT j.status,d.reason FROM memory_jobs j JOIN memory_job_dispositions d ON d.job_id=j.id WHERE j.id=?", j.ID).Scan(&state, &reason); err != nil || state != "discarded" || reason != "run_not_completed" {
		t.Fatalf("%s %s %v", state, reason, err)
	}
}

func TestMemoryStage4DeletedSessionRetainsNoJobSkip(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	if _, err := s.db.Exec("DELETE FROM memory_job_leases; DELETE FROM memory_jobs; UPDATE memory_inputs SET job_id='',decision='disabled'"); err != nil {
		t.Fatal(err)
	}
	set, err := s.CaptureFlush(t.Context(), j.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DELETE FROM harness_sessions; DELETE FROM harness_runs"); err != nil {
		t.Fatal(err)
	}
	status, err := s.ObserveFlush(t.Context(), set)
	if err != nil || status.Skipped != 1 || status.Blocked != 0 {
		t.Fatalf("resolved skip became blocked: %+v %v", status, err)
	}
}
