package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type collectorFixture struct {
	*fixture
	runtime *hr.Store
	ledger  *budget.Ledger
	read    skills.ReadObservation
}

func newCollectorFixture(t *testing.T) *collectorFixture {
	t.Helper()
	f := &collectorFixture{fixture: newFixture(t, nil)}
	var err error
	f.runtime, err = hr.NewStore(context.Background(), f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.ledger, err = budget.New(f.db.DB(), budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.registry.Snapshot(context.Background(), harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.WithReadObserver(func(_ context.Context, read skills.ReadObservation) error { f.read = read; return nil }).Get(context.Background(), "research")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.service.Close() })
	return f
}

func (f *collectorFixture) run(t *testing.T, root string, reads int, status string, uncertain bool) harness.RunRequest {
	t.Helper()
	ctx := context.Background()
	x, err := f.runtime.CreateSession(ctx, f.cfg.Sources[0].Workspace, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: x, RunID: hr.NewID(), InputID: hr.NewID(), RootBudgetID: root}
	if root == "" {
		req.RootBudgetID = req.RunID
	}
	if err = f.runtime.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if root == "" {
		err = f.ledger.CreateRoot(ctx, budget.RootSpec{RootBudgetID: req.RootBudgetID, RootRunID: req.RunID, SessionID: x.ID})
	} else {
		err = f.ledger.BindMember(ctx, budget.MemberBinding{RootBudgetID: root, MemberID: req.RunID, SessionID: x.ID, OriginRunID: root, ParentMemberID: root, Kind: "task"})
	}
	if err != nil {
		t.Fatal(err)
	}
	scope := budget.Scope{RootBudgetID: req.RootBudgetID, MemberID: req.RunID, SessionID: x.ID, AttemptID: req.RunID, Fence: 1}
	if err = f.ledger.BeginAttempt(ctx, scope); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < reads; i++ {
		kind := "skill_load"
		if i == 1 {
			kind = "support_read"
		}
		r := f.read
		// The real snapshot/brokers are covered by runtime and SDK tests. Here
		// insert their digest-only event envelope to isolate collector faults.
		_, err = f.db.DB().Exec(`INSERT INTO harness_skill_reads(run_id,session_id,root_id,attempt_id,role_id,source_id,source_key,scope,name,version,path,file_hash,bytes,kind,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			req.RunID, x.ID, req.RootBudgetID, scope.AttemptID, "", r.Ref.SourceID, r.Ref.SourceIdentity, r.Scope, r.Ref.Name, r.Ref.Version, r.File.Path, r.File.Hash, r.File.Bytes, kind, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	if uncertain {
		if _, err = f.runtime.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "tool_start", ToolCallID: "external", ToolName: "write_file", Status: "pending", Arguments: []byte(`{"path":"fixture"}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err = f.runtime.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: req.RunID, Kind: "tool_execute", ToolCallID: "external", ToolName: "write_file", Status: "in_progress"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.ledger.EndAttempt(ctx, scope, budget.Outcome(status)); err != nil {
		t.Fatal(err)
	}
	var runErr error
	reason := "end_turn"
	if status == "failed" {
		runErr = errors.New("PRIVATE_PROVIDER_DETAILS")
	}
	if status == "cancelled" {
		reason = "cancelled"
	}
	if err = f.runtime.Finish(ctx, req.RunID, reason, runErr); err != nil {
		t.Fatal(err)
	}
	return req
}

func (f *collectorFixture) drain(t *testing.T, limit int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		worked, err := f.service.CollectEvidence(context.Background(), limit)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("collector failed to quiesce")
}

func TestCollectorPagesDeduplicateRootsAndKeepUnattributedOutcomes(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 2, "completed", false)
	f.run(t, root.RunID, 2, "failed", true)
	f.run(t, root.RunID, 1, "cancelled", false)
	if worked, err := f.service.CollectEvidence(ctx, 1); err != nil || !worked {
		t.Fatalf("step=%v %v", worked, err)
	}
	page, err := f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Signals) != 1 || page.Signals[0].ReadCount != 1 || !page.Signals[0].CollectionPending {
		t.Fatalf("partial=%+v %v", page, err)
	}
	id := page.Signals[0].ID
	f.drain(t, 1)
	page, err = f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Signals) != 1 {
		t.Fatalf("page=%+v %v", page, err)
	}
	s := page.Signals[0]
	if s.ID != id || s.RootID != root.RunID || s.RootSessionID != root.Session.ID || s.ReadCount != 5 || s.RunCount != 3 || s.CompletedRuns != 1 || s.FailedRuns != 1 || s.CancelledRuns != 1 || s.UncertainRuns != 1 || s.CollectionPending || s.Attribution != "unassessed" {
		t.Fatalf("signal=%+v", s)
	}
	var all []harness.SkillSignalRead
	cursor := ""
	for {
		evidence, err := f.service.SignalEvidence(ctx, id, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evidence.Reads...)
		cursor = evidence.Next
		if cursor == "" {
			break
		}
	}
	if len(all) != 5 || f.calls.Load() != 0 {
		t.Fatalf("reads=%d review model calls=%d", len(all), f.calls.Load())
	}
	before := page.CatalogVersion
	f.drain(t, 2)
	page, err = f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || page.CatalogVersion != before || page.Signals[0].ReadCount != 5 {
		t.Fatal("replay changed evidence", err)
	}
}

func TestCollectorPageRollbackRetriesAndDoesNotChangeCompletedRun(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 2, "completed", false)
	if _, err := f.db.DB().Exec(`CREATE TRIGGER collector_fault BEFORE INSERT ON df_skill_signal_reads BEGIN SELECT RAISE(ABORT,'PRIVATE_FAILURE'); END`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 1; i <= maxCollectionAttempts; i++ {
		claim, ok, err := f.service.claimCollection(ctx, now)
		if err != nil || !ok || claim.attempts != i {
			t.Fatalf("claim=%+v %v %v", claim, ok, err)
		}
		if err = f.service.collectPage(ctx, claim, 2, now); err == nil {
			t.Fatal("expected injected page failure")
		}
		if err = f.service.failCollection(ctx, claim, "storage_error", false, now); err != nil {
			t.Fatal(err)
		}
		if _, worked, err := f.service.claimCollection(ctx, now); err != nil || worked {
			t.Fatalf("retry ignored backoff/limit: %v %v", worked, err)
		}
		now = now.Add(5 * time.Second)
	}
	status, err := f.service.CollectorStatus(ctx)
	if err != nil || status.Failed != 1 || status.Pending != 1 || len(status.Jobs) != 1 {
		t.Fatalf("status=%+v %v", status, err)
	}
	job := status.Jobs[0]
	if job.Attempts != 3 || job.ReadCursor != 0 || job.ErrorCode != "storage_error" {
		t.Fatalf("job=%+v", job)
	}
	var current string
	var signals int
	if err = f.db.DB().QueryRow(`SELECT status FROM harness_runs WHERE id=?`, run.RunID).Scan(&current); err != nil || current != "completed" {
		t.Fatal("collector changed user outcome", current, err)
	}
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_signals`).Scan(&signals); err != nil || signals != 0 {
		t.Fatal("page transaction left partial signal", signals, err)
	}
	if err = f.service.RetryCollection(ctx, job.OutcomeID, job.StateVersion-1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale retry accepted", err)
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER collector_fault`); err != nil {
		t.Fatal(err)
	}
	if err = f.service.RetryCollection(ctx, job.OutcomeID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	if err = f.service.RetryCollection(ctx, job.OutcomeID, job.StateVersion); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("duplicate retry reset a running generation", err)
	}
	f.drain(t, 1)
	status, err = f.service.CollectorStatus(ctx)
	if err != nil || status.Failed != 0 || status.Pending != 0 || status.Completed != 1 {
		t.Fatalf("recovered=%+v %v", status, err)
	}
}

func TestCollectorRestartResumesCursorAndRejectsExpiredWorker(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	f.run(t, "", 2, "completed", false)
	if _, err := f.service.CollectEvidence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old, ok, err := f.service.claimCollection(ctx, now)
	if err != nil || !ok || old.cursor == 0 {
		t.Fatalf("claim=%+v %v", old, err)
	}
	reopened, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	later := now.Add(collectionLease + time.Second)
	claim, ok, err := reopened.claimCollection(ctx, later)
	if err != nil || !ok || claim.cursor != old.cursor || claim.attempts != 2 {
		t.Fatalf("recovered claim=%+v %v", claim, err)
	}
	if err = f.service.collectPage(ctx, old, 1, later); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("expired worker committed", err)
	}
	if err = f.service.failCollection(ctx, old, "storage_error", false, later); err != nil {
		t.Fatal(err)
	}
	if err = reopened.collectPage(ctx, claim, 1, later); err != nil {
		t.Fatal(err)
	}
	page, err := reopened.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Signals) != 1 || page.Signals[0].ReadCount != 2 || page.Signals[0].CollectionPending {
		t.Fatalf("reopened=%+v %v", page, err)
	}
}

func TestCollectorQueriesBindIdentityCursorsAndRegistryVisibility(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	first := f.run(t, "", 2, "completed", false)
	f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	page, err := f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 1})
	if err != nil || page.Next == "" {
		t.Fatalf("page=%+v %v", page, err)
	}
	if _, err = f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 1, Cursor: page.Next, Name: "other"}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("cross-filter cursor accepted", err)
	}
	evidence, err := f.service.SignalEvidence(ctx, signalID(first.RunID, f.read.Ref.SourceIdentity, "research", f.read.Ref.Version), "", 1)
	if err != nil || evidence.Next == "" {
		t.Fatalf("evidence=%+v %v", evidence, err)
	}
	f.run(t, first.RunID, 1, "completed", false)
	f.drain(t, 128)
	if _, err = f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 1, Cursor: page.Next}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale signal page accepted", err)
	}
	if _, err = f.service.SignalEvidence(ctx, evidence.Signal.ID, evidence.Next, 1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale evidence page accepted", err)
	}
	registry, err := skills.NewRegistry(ctx, harness.SkillsConfig{}, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	other, err := New(ctx, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	hidden, err := other.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(hidden.Signals) != 0 {
		t.Fatalf("removed source visible: %+v %v", hidden, err)
	}
	if _, err = other.SignalEvidence(ctx, evidence.Signal.ID, "", 10); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("removed source detail visible", err)
	}
}

func TestCollectorRejectsCorruptEvidenceAndKeepsOtherWorkMoving(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	if _, err := f.db.DB().Exec(`UPDATE harness_skill_reads SET file_hash=? WHERE run_id=?`, strings.Repeat("0", 64), run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CollectEvidence(ctx, 128); !errors.Is(err, errCollectionEvidence) {
		t.Fatal("corrupt evidence accepted", err)
	}
	f.run(t, "", 0, "cancelled", false)
	if _, err := f.service.CollectEvidence(ctx, 128); err != nil {
		t.Fatal("backoff blocked later job", err)
	}
	status, err := f.service.CollectorStatus(ctx)
	if err != nil || status.Completed != 1 || status.Pending != 1 || status.Jobs[0].ErrorCode != "invalid_evidence" {
		t.Fatalf("status=%+v %v", status, err)
	}
	var count int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_signals`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invented signals=%d %v", count, err)
	}
}

func TestCollectorWorkerJoinsAndDeletionCascadesPrivateEvidence(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 2, "completed", false)
	if err := f.service.StartCollector(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := f.service.CollectorStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status.Pending == 0 && status.Completed == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("collector did not process durable outbox")
		case <-ticker.C:
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.StartCollector(); err == nil {
		t.Fatal("closed collector restarted")
	}
	// A purge's explicit deletes and foreign-key cascades share one transaction.
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM harness_skill_reads WHERE session_id=?`, `DELETE FROM harness_skill_outcomes WHERE session_id=?`, `DELETE FROM df_skill_signals WHERE session_id=?`} {
		if _, err = tx.Exec(q, run.Session.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"df_skill_signals", "df_skill_signal_reads", "df_skill_collection_jobs"} {
		var count int
		if err = f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("retained %s=%d %v", table, count, err)
		}
	}
}
