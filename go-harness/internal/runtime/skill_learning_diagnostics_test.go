package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningDiagnosticsLiveReasonsMetadataAndBounds(t *testing.T) {
	ctx := context.Background()
	s, session, _ := newLearningTest(t, true)
	old := learningTestRun(t, s, session, "PRIVATE_OLD_PROMPT")
	if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='needs_reconciliation' WHERE id=?`, old.RunID); err != nil {
		t.Fatal(err)
	}
	later := learningTestRun(t, s, session, "PRIVATE_LATER_PROMPT")
	learningTestFinish(t, s, later, "end_turn")
	d, err := s.Store.Learning.Diagnostics(ctx, session.ID, 100)
	if err != nil || d.Queue.Pending != 1 || d.Queue.Ready != 0 || d.Queue.Blocked != 1 || len(d.Inputs) != 2 {
		t.Fatal(d, err)
	}
	if d.Inputs[0].RunID != old.RunID || d.Inputs[0].Reason != "needs_reconciliation" || d.Inputs[1].Reason != "idle_window" {
		t.Fatal(d.Inputs)
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), "PRIVATE_") || strings.Contains(string(data), session.CWD) {
		t.Fatal("diagnostics exposed source contents")
	}
	// Persisted reasons may be empty/stale. Current readiness must recover once
	// the debounce expires despite an older unfinished turn in this episode.
	time.Sleep(1050 * time.Millisecond)
	status, err := s.Store.Learning.Status(ctx)
	if err != nil || status.Queue.Ready != 1 || status.Queue.Blocked != 0 || status.Pending != 1 || status.Running {
		t.Fatal(status, err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 1)
	if err != nil || !d.Truncated || len(d.Inputs) != 1 || d.Queue.Ready != 1 {
		t.Fatal(d, err)
	}
	if _, err = s.Store.Learning.Diagnostics(ctx, session.ID, 101); err == nil {
		t.Fatal("unbounded diagnostics accepted")
	}
	if worked, err := s.Store.Learning.QueueOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	status, err = s.Store.Learning.Status(ctx)
	if err != nil || status.Queue.Queued != 1 || status.Queue.Ready != 0 || status.Queue.Pending != 0 {
		t.Fatal(status, err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 100)
	if err != nil || d.Inputs[1].Reason != "awaiting_worker" {
		t.Fatal(d, err)
	}
	active := learningTestRun(t, s, session, "PRIVATE_ACTIVE_PROMPT")
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var queuedReason string
	for _, input := range d.Inputs {
		if input.State == "queued" {
			queuedReason = input.Reason
		}
	}
	if queuedReason != "source_active" {
		t.Fatal("queued source gate is invisible", d.Inputs)
	}
	learningTestFinish(t, s, active, "end_turn")
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range d.Inputs {
		if input.State == "queued" && input.Reason != "awaiting_worker" {
			t.Fatal("queued readiness failed to recover", input)
		}
	}
}

func TestSkillLearningExtractionRetryPreservesAccountingAndGuards(t *testing.T) {
	ctx := context.Background()
	s, session, extractor := newLearningTest(t, true)
	run := learningTestRun(t, s, session, "PRIVATE_RETRY_PROMPT")
	learningTestFinish(t, s, run, "end_turn")
	learningTestQueue(t, s)
	extractor.bad = true
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	page, err := s.Store.Learning.Jobs(ctx, session.ID, "", 10)
	if err != nil || len(page.Jobs) != 1 || page.Jobs[0].State != "failed" {
		t.Fatal(page, err)
	}
	job := page.Jobs[0]
	// The fixture's paid extraction produced an invalid candidate. Model a
	// transient response failure without replacing its job, source or ledger.
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET error_code='model_failed' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	control, err := s.Store.Learning.Control(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := harness.SkillLearningExtractionRetryRequest{JobID: job.ID, ExpectedEpoch: control.Epoch, ExpectedStateVersion: job.StateVersion}
	d, err := s.Store.Learning.Diagnostics(ctx, session.ID, 10)
	if err != nil || len(d.Jobs) != 1 || !d.Jobs[0].Retryable || d.Queue.Failed != 1 || d.Queue.FailedJobs != 1 {
		t.Fatal(d, err)
	}
	for _, field := range []string{"epoch", "version", "session"} {
		bad, target := req, session.ID
		switch field {
		case "epoch":
			bad.ExpectedEpoch++
		case "version":
			bad.ExpectedStateVersion++
		case "session":
			target = "other"
		}
		if _, err := s.Store.Learning.RetryExtraction(ctx, target, bad); err == nil {
			t.Fatal("stale/foreign retry accepted", field)
		}
	}
	var saved []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_extractions WHERE job_id=?`, job.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_learning_extractions SET payload='{}' WHERE job_id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 10)
	if err != nil || d.Jobs[0].Retryable || d.Jobs[0].RetryReason != "source_unavailable" {
		t.Fatal(d, err)
	}
	if _, err = s.Store.Learning.RetryExtraction(ctx, session.ID, req); err == nil {
		t.Fatal("corrupt extraction admitted")
	}
	if _, err = s.Store.db.Exec(`UPDATE df_learning_extractions SET payload=? WHERE job_id=?`, saved, job.ID); err != nil {
		t.Fatal(err)
	}
	var sharedDay string
	var sharedCalls int64
	if err = s.Store.db.QueryRow(`SELECT day,model_calls FROM df_skill_maintenance_days`).Scan(&sharedDay, &sharedCalls); err != nil {
		t.Fatal(err)
	}
	// Other maintenance producers can exhaust the shared budget while the
	// learning-only sublimit still has room. Preflight must account for both.
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=? WHERE day=?`, learningTestOptions().Budget.MaxDailyModelCalls, sharedDay); err != nil {
		t.Fatal(err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 10)
	if err != nil || d.Jobs[0].Retryable || d.Jobs[0].RetryReason != "budget_exhausted" {
		t.Fatal(d, err)
	}
	if _, err = s.Store.Learning.RetryExtraction(ctx, session.ID, req); err == nil {
		t.Fatal("retry consumed an attempt against exhausted shared budget")
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=? WHERE day=?`, sharedCalls, sharedDay); err != nil {
		t.Fatal(err)
	}
	for _, condition := range []struct {
		sql, reason string
		value       any
	}{
		{`UPDATE df_skill_maintenance_jobs SET attempts=? WHERE id=?`, "attempts_exhausted", 3},
		{`UPDATE df_skill_maintenance_jobs SET deadline_ns=? WHERE id=?`, "deadline_expired", time.Now().Add(-time.Hour).UnixNano()},
	} {
		if _, err = s.Store.db.Exec(condition.sql, condition.value, job.ID); err != nil {
			t.Fatal(err)
		}
		d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 10)
		if err != nil || d.Jobs[0].Retryable || d.Jobs[0].RetryReason != condition.reason {
			t.Fatal(d, err)
		}
		if _, err = s.Store.Learning.RetryExtraction(ctx, session.ID, req); err == nil {
			t.Fatal("retry reset lifetime", condition.reason)
		}
		if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET attempts=?,deadline_ns=? WHERE id=?`, job.Attempts, job.Deadline.UnixNano(), job.ID); err != nil {
			t.Fatal(err)
		}
	}
	var calls, charged int64
	if err = s.Store.db.QueryRow(`SELECT count(*),sum(charged_tokens) FROM df_skill_maintenance_calls`).Scan(&calls, &charged); err != nil {
		t.Fatal(err)
	}
	retried, err := s.Store.Learning.RetryExtraction(ctx, session.ID, req)
	if err != nil || retried.State != "queued" || retried.Attempts != job.Attempts || !retried.Deadline.Equal(job.Deadline) {
		t.Fatal(retried, err)
	}
	replayed, err := s.Store.Learning.RetryExtraction(ctx, session.ID, req)
	if err != nil || replayed != retried {
		t.Fatal("retry not idempotent", replayed, err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=? WHERE day=?`, learningTestOptions().Budget.MaxDailyModelCalls, sharedDay); err != nil {
		t.Fatal(err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 10)
	if err != nil || len(d.Inputs) != 1 || d.Inputs[0].State != "queued" || d.Inputs[0].Reason != "budget_exhausted" {
		t.Fatal("queued diagnostics hid shared-budget exhaustion", d, err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_days SET model_calls=? WHERE day=?`, sharedCalls, sharedDay); err != nil {
		t.Fatal(err)
	}
	var afterCalls, afterCharged int64
	if err = s.Store.db.QueryRow(`SELECT count(*),sum(charged_tokens) FROM df_skill_maintenance_calls`).Scan(&afterCalls, &afterCharged); err != nil || calls != afterCalls || charged != afterCharged {
		t.Fatal("retry changed accounting", err)
	}
	extractor.bad = false
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	page, err = s.Store.Learning.Jobs(ctx, session.ID, "", 10)
	if err != nil || page.Jobs[0].State != "completed" || page.Jobs[0].Attempts != job.Attempts+1 {
		t.Fatal(page, err)
	}
	if _, err = s.Store.Learning.RetryExtraction(ctx, session.ID, req); err == nil {
		t.Fatal("committed output was requeued")
	}
	// A durable result followed by a failed ACK must never be treated as a
	// request to perform another paid extraction.
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='failed',error_code='storage_error' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	d, err = s.Store.Learning.Diagnostics(ctx, session.ID, 10)
	if err != nil || d.Jobs[0].Retryable || d.Jobs[0].RetryReason != "result_committed" {
		t.Fatal(d, err)
	}
	if learningCount(t, s.Store, "harness_learning_inputs") != 1 || learningCount(t, s.Store, "df_skill_maintenance_jobs") != 1 {
		t.Fatal("retry duplicated foreground work or job")
	}
}
