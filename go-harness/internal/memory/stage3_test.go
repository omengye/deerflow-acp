package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func TestMemoryStage3KnownRecoveryPreservesOtherUnknownMember(t *testing.T) {
	s, scope := personalStore(t)
	stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.ReserveJobCall(t.Context(), j, "fixture", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchJobCall(t.Context(), j, g); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveJobResponse(t.Context(), j, JobResponse{Raw: stage1City(t), Complete: true, Usage: harness.Usage{TotalTokens: 15}}); err != nil {
		t.Fatal(err)
	}
	// Host binds separate work under the same root. Reconciliation must not clear
	// that member's interruption while settling the saved memory response.
	other := budget.Scope{RootBudgetID: j.RootBudgetID, MemberID: "other", SessionID: j.SessionID, AttemptID: "other-attempt", Fence: 1}
	if err = s.ledger.BindMember(t.Context(), budget.MemberBinding{RootBudgetID: j.RootBudgetID, MemberID: "other", SessionID: j.SessionID, OriginRunID: j.RunID, Kind: "run"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.BeginAttempt(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.EndAttempt(t.Context(), other, budget.OutcomeInterrupted); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ledger.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil || snap.BlockedReason != "unknown" || snap.HeldTokens != 0 || snap.SpentTokens != 15 {
		t.Fatalf("other member lost: %+v %v", snap, err)
	}
}

func TestMemoryStage3JobBindingAndUndispatchedCleanupAreAtomic(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	// Restore the accepted fixture's foreground attempt solely to replay enqueue.
	if _, err := s.db.Exec("DELETE FROM memory_job_leases; DELETE FROM memory_jobs; DELETE FROM budget_members WHERE kind='memory_job'; UPDATE memory_inputs SET decision='pending',job_id=''; UPDATE budget_attempts SET state='active',outcome='' WHERE id='attempt-run'; CREATE TRIGGER fail_lease BEFORE INSERT ON memory_job_leases BEGIN SELECT RAISE(ABORT,'lease_failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(t.Context(), Job{RunID: j.RunID, InputID: j.InputID, AttemptID: "attempt-run"}); err == nil {
		t.Fatal("enqueue fault lost")
	}
	var members, jobs int
	if err := s.db.QueryRow("SELECT count(*) FROM budget_members WHERE kind='memory_job'").Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM memory_jobs").Scan(&jobs); err != nil || members != 0 || jobs != 0 {
		t.Fatalf("partial binding: members=%d jobs=%d %v", members, jobs, err)
	}
}

func TestMemoryStage3NoLedgerAndLegacyResultAPIsFailClosed(t *testing.T) {
	s, scope := personalStore(t)
	stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveJobResult(t.Context(), j.ID, stage1City(t)); !errors.Is(err, budget.ErrScope) {
		t.Fatal("unmetered result accepted")
	}
	if err = s.FinishJob(t.Context(), j.ID, false); !errors.Is(err, budget.ErrScope) {
		t.Fatal("unfenced finish accepted")
	}
	if err = s.ApplyJob(t.Context(), j, stage1City(t)); !errors.Is(err, budget.ErrScope) {
		t.Fatal("unmetered publication accepted")
	}
	// Keep the general ledger's unknown-member gate intact.
	if err = transact(t.Context(), s.db, func(tx *sql.Tx) error {
		return s.CheckJobBudgetEffectTx(context.Background(), tx, budget.Scope{RootBudgetID: j.RootBudgetID, MemberID: "unregistered", SessionID: j.SessionID, AttemptID: j.WorkerAttemptID, Fence: j.Fence})
	}); err == nil {
		t.Fatal("unregistered member accepted")
	}
	s.SetJobLedger(nil)
	if _, err = s.ReserveJobCall(t.Context(), j, "request", 10, 100); !errors.Is(err, budget.ErrScope) {
		t.Fatal("missing ledger accepted")
	}
	if err = s.ApplyJob(t.Context(), j, stage1City(t)); !errors.Is(err, budget.ErrScope) {
		t.Fatal("missing ledger allowed publication")
	}
}

func TestMemoryStage3RootStopFencesAlreadyPaidResult(t *testing.T) {
	s, scope := personalStore(t)
	stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = stage3SaveFixture(t, s, j, stage1City(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("CREATE TABLE harness_task_tree_stops(root_id TEXT,node_id TEXT); INSERT INTO harness_task_tree_stops VALUES('run','')"); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyJob(t.Context(), j, stage1City(t)); err != nil {
		t.Fatal(err)
	}
	var facts int
	var status, reason string
	if err = s.db.QueryRow("SELECT count(*) FROM memory_facts").Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT status,error_code FROM memory_jobs WHERE id=?", j.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ledger.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil || facts != 0 || status != "discarded" || reason != "cancelled" || snap.SpentTokens != 15 || snap.HeldTokens != 0 {
		t.Fatalf("facts=%d status=%s reason=%s budget=%+v %v", facts, status, reason, snap, err)
	}
}

func TestMemoryStage3UnbudgetedLegacyResultIsNeverReplayed(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			s, scope := personalStore(t)
			j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			if _, err := s.db.Exec("UPDATE memory_jobs SET payload=json_remove(CAST(payload AS TEXT),'$.budget_policy'),status=?,result=? WHERE id=?", status, stage1City(t), j.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverJobs(t.Context()); err != nil {
				t.Fatal(err)
			}
			var state, raw, usage string
			err := s.db.QueryRow("SELECT j.status,j.result,d.usage_state FROM memory_jobs j JOIN memory_job_dispositions d ON d.job_id=j.id WHERE j.id=?", j.ID).Scan(&state, &raw, &usage)
			if err != nil || state != "discarded" || raw != stage1City(t) || usage != "legacy_usage_unavailable" {
				t.Fatalf("state=%s raw=%s usage=%s %v", state, raw, usage, err)
			}
			if _, err = s.ClaimJob(t.Context()); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestMemoryStage3TemporaryHoldKeepsJobEligible(t *testing.T) {
	s, scope := personalStore(t)
	stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	for n := 0; n < 4; n++ {
		j, err := s.ClaimJob(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = s.AbortJob(t.Context(), j, &budget.LimitError{Resource: "tokens", Temporary: true}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("UPDATE memory_jobs SET available_at='2000-01-01' WHERE id=?", j.ID); err != nil {
			t.Fatal(err)
		}
	}
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatalf("temporary hold stranded job: %v", err)
	}
	if j.Fence != 5 {
		t.Fatal("retry reused the old fence")
	}
}
func TestMemoryStage3BudgetMigrationRollsBack(t *testing.T) {
	s, _ := personalStore(t)
	if _, err := s.db.Exec("DROP TABLE memory_value_identities; DROP TABLE memory_index_failures; DROP TABLE memory_index_backoff; DROP TABLE memory_job_leases; DELETE FROM memory_schema_migrations WHERE version>=3; CREATE TRIGGER stage3_fail BEFORE INSERT ON memory_schema_migrations WHEN NEW.version=3 BEGIN SELECT RAISE(ABORT,'budget_migration'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), s.db); err == nil {
		t.Fatal("migration failure lost")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='memory_job_leases'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("half migration: %d %v", n, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER stage3_fail"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := New(t.Context(), s.db); err != nil {
			t.Fatal(err)
		}
	}
}
func TestMemoryStage3CancelledReservedCallReleasesOnlyUndispatchedHold(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "reserved", true: "dispatched"}[dispatched], func(t *testing.T) {
			s, scope := personalStore(t)
			stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			j, err := s.ClaimJob(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			g, err := s.ReserveJobCall(t.Context(), j, "request", 10, 100)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched {
				if _, err = s.DispatchJobCall(t.Context(), j, g); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.db.Exec("UPDATE memory_jobs SET status='discarded' WHERE id=?", j.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.AbortJob(t.Context(), j, harness.ErrExecutionConflict); err != nil {
				t.Fatal(err)
			}
			snap, err := s.ledger.Snapshot(t.Context(), j.RootBudgetID)
			if err != nil {
				t.Fatal(err)
			}
			if !dispatched && (snap.HeldTokens != 0 || snap.BlockedReason != "") || dispatched && (snap.HeldTokens != 110 || snap.BlockedReason != "unknown") {
				t.Fatalf("dispatched=%v snapshot=%+v", dispatched, snap)
			}
		})
	}
}
func TestMemoryStage3HeartbeatRenewsLeaseAndReportsTimeLimit(t *testing.T) {
	s, scope := personalStore(t)
	stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(time.Second).UnixNano()
	if _, err = s.db.Exec("UPDATE memory_job_leases SET lease_ns=? WHERE job_id=?", before, j.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.HeartbeatJob(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err = s.db.QueryRow("SELECT lease_ns FROM memory_job_leases WHERE job_id=?", j.ID).Scan(&after); err != nil || after <= before {
		t.Fatalf("lease=%d %v", after, err)
	}
	if _, err = s.db.Exec("UPDATE budget_roots SET blocked_reason='time' WHERE id=?", j.RootBudgetID); err != nil {
		t.Fatal(err)
	}
	if err = s.HeartbeatJob(t.Context(), j); err == nil {
		t.Fatal("time budget ignored")
	}
}
