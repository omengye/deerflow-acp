package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type fixtureClock struct{ nanos atomic.Int64 }

func newClock() *fixtureClock {
	c := &fixtureClock{}
	c.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fixtureClock) now() time.Time      { return time.Unix(0, c.nanos.Load()) }
func (c *fixtureClock) add(d time.Duration) { c.nanos.Add(int64(d)) }

func openBudget(t *testing.T, path string, cfg Config) *Ledger {
	t.Helper()
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	l, err := New(store.DB(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func budgetFixture(t *testing.T, limits harness.BudgetLimits) (*Ledger, *fixtureClock, Scope) {
	t.Helper()
	clock := newClock()
	l := openBudget(t, filepath.Join(t.TempDir(), "budget.db"), Config{Now: clock.now, LeaseDuration: time.Minute})
	if err := l.CreateRoot(context.Background(), RootSpec{RootBudgetID: "root", RootRunID: "run", SessionID: "parent", Limits: limits}); err != nil {
		t.Fatal(err)
	}
	s := Scope{RootBudgetID: "root", MemberID: "run", SessionID: "parent", AttemptID: "run/1", Fence: 1}
	if err := l.BeginAttempt(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return l, clock, s
}
func snapshot(t *testing.T, l *Ledger, id string) Snapshot {
	t.Helper()
	s, err := l.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConcurrentLedgersRespectSharedCaps(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	path := filepath.Join(t.TempDir(), "shared.db")
	a := openBudget(t, path, Config{Now: clock.now})
	b := openBudget(t, path, Config{Now: clock.now})
	if err := a.CreateRoot(ctx, RootSpec{RootBudgetID: "root", RootRunID: "run", SessionID: "parent", Limits: harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 5, MaxTokens: 80, MaxOutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	s := Scope{RootBudgetID: "root", MemberID: "run", SessionID: "parent", AttemptID: "run/1", Fence: 1}
	if err := a.BeginAttempt(ctx, s); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	var models, tools atomic.Int64
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := a
			if i%2 == 0 {
				l = b
			}
			var err error
			if i < 20 {
				_, err = l.ReserveModel(ctx, s, ModelRequest{OperationID: fmt.Sprintf("model-%d", i), Digest: "input", InputTokens: 5, MaxOutputTokens: 5})
				if err == nil {
					models.Add(1)
				}
			} else {
				_, err = l.ReserveTool(ctx, s, ToolRequest{OperationID: fmt.Sprintf("tool-%d", i), Digest: "input"})
				if err == nil {
					tools.Add(1)
				}
			}
			if err != nil {
				var limit *LimitError
				if !errors.As(err, &limit) {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got := snapshot(t, a, "root")
	if models.Load() != 8 || tools.Load() != 5 || got.ModelCalls != 8 || got.ToolCalls != 5 || got.HeldTokens != 80 || got.SpentTokens != 0 {
		t.Fatalf("shared caps escaped: model=%d tool=%d snapshot=%+v", models.Load(), tools.Load(), got)
	}
}

func TestReservationDispatchSettlementIdempotency(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{MaxTokens: 100})
	ctx := context.Background()
	req := ModelRequest{OperationID: "operation", Digest: "input", InputTokens: 10, MaxOutputTokens: 20}
	grant, err := l.ReserveModel(ctx, s, req)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := l.ReserveModel(ctx, s, req)
	if err != nil || duplicate != grant {
		t.Fatalf("reserve replay: %+v %v", duplicate, err)
	}
	req.Digest = "changed"
	if _, err = l.ReserveModel(ctx, s, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed intent: %v", err)
	}
	if first, err := l.MarkDispatched(ctx, grant); err != nil || !first {
		t.Fatalf("first dispatch: %v %v", first, err)
	}
	if first, err := l.MarkDispatched(ctx, grant); err != nil || first {
		t.Fatalf("dispatch replay: %v %v", first, err)
	}
	settlement := Settlement{Complete: true, Usage: harness.Usage{InputTokens: 7, OutputTokens: 8, TotalTokens: 15}}
	if _, err = l.Settle(ctx, grant, settlement); err != nil {
		t.Fatal(err)
	}
	if _, err = l.Settle(ctx, grant, settlement); err != nil {
		t.Fatal(err)
	}
	settlement.Usage.OutputTokens++
	if _, err = l.Settle(ctx, grant, settlement); !errors.Is(err, ErrConflict) {
		t.Fatalf("settlement conflict: %v", err)
	}
	got := snapshot(t, l, "root")
	if got.ModelCalls != 1 || got.SpentTokens != 15 || got.HeldTokens != 0 {
		t.Fatalf("duplicate charge: %+v", got)
	}
}

func TestScopeOriginSessionAndOldFence(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{})
	ctx := context.Background()
	if err := l.CreateRoot(ctx, RootSpec{RootBudgetID: "other-root", RootRunID: "other-run", SessionID: "other"}); err != nil {
		t.Fatal(err)
	}
	for _, member := range []MemberBinding{
		{RootBudgetID: "other-root", MemberID: "child", SessionID: "child-session", OriginRunID: "run", ParentMemberID: "run", Kind: "task"},
		{RootBudgetID: "root", MemberID: "child", SessionID: "other", OriginRunID: "run", Kind: "run"},
		{RootBudgetID: "root", MemberID: "child", SessionID: "child-session", OriginRunID: "run", ParentMemberID: "other-run", Kind: "task"},
	} {
		if err := l.BindMember(ctx, member); err == nil {
			t.Fatalf("accepted inconsistent binding: %+v", member)
		}
	}
	wrong := s
	wrong.SessionID = "other"
	if _, err := l.ReserveTool(ctx, wrong, ToolRequest{OperationID: "wrong-session", Digest: "x"}); !errors.Is(err, ErrScope) {
		t.Fatalf("scope session: %v", err)
	}
	if err := l.EndAttempt(ctx, s, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	next := s
	next.Fence++
	next.AttemptID = "run/2"
	if err := l.BeginAttempt(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveTool(ctx, s, ToolRequest{OperationID: "stale", Digest: "x"}); !errors.Is(err, ErrScope) {
		t.Fatalf("stale fence reserved: %v", err)
	}
	if err := l.BeginAttempt(ctx, s); err == nil {
		t.Fatal("old attempt restarted")
	}
	if _, err := l.ReserveTool(ctx, next, ToolRequest{OperationID: "current", Digest: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartUnknownPreservesHoldAndBoundsClock(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	path := filepath.Join(t.TempDir(), "restart.db")
	l := openBudget(t, path, Config{Now: clock.now, LeaseDuration: 10 * time.Second})
	if err := l.CreateRoot(ctx, RootSpec{RootBudgetID: "root", RootRunID: "run", SessionID: "parent", Limits: harness.BudgetLimits{MaxTokens: 100}}); err != nil {
		t.Fatal(err)
	}
	s := Scope{RootBudgetID: "root", MemberID: "run", SessionID: "parent", AttemptID: "run/1", Fence: 1}
	if err := l.BeginAttempt(ctx, s); err != nil {
		t.Fatal(err)
	}
	grant, err := l.ReserveModel(ctx, s, ModelRequest{OperationID: "inflight", Digest: "input", InputTokens: 10, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := l.MarkDispatched(ctx, grant); err != nil || !first {
		t.Fatal(err)
	}
	clock.add(time.Hour)
	reopened := openBudget(t, path, Config{Now: clock.now, LeaseDuration: 10 * time.Second})
	if err = reopened.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, reopened, "root")
	if got.HeldTokens != 30 || got.SpentTokens != 0 || got.BlockedReason != "unknown" || got.Elapsed != 10*time.Second {
		t.Fatalf("restart reset/spent runaway: %+v", got)
	}
	if _, err = reopened.ReserveTool(ctx, s, ToolRequest{OperationID: "new", Digest: "x"}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown accepted new effect: %v", err)
	}
	if first, err := reopened.MarkDispatched(ctx, grant); first || !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown replay: %v %v", first, err)
	}
	// A late accounting-only settlement may report the already incurred charge.
	if _, err = reopened.Settle(ctx, grant, Settlement{Complete: true, Usage: harness.Usage{TotalTokens: 12}}); err != nil {
		t.Fatal(err)
	}
	got = snapshot(t, reopened, "root")
	if got.HeldTokens != 0 || got.SpentTokens != 12 || got.BlockedReason != "unknown" {
		t.Fatalf("late settlement: %+v", got)
	}
}

func TestClockUnionQueuedPauseAndBackwardTime(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	l := openBudget(t, filepath.Join(t.TempDir(), "clock.db"), Config{Now: clock.now, LeaseDuration: time.Minute})
	if err := l.CreateRoot(ctx, RootSpec{RootBudgetID: "root", RootRunID: "run", SessionID: "parent"}); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour)
	if got := snapshot(t, l, "root"); got.Elapsed != 0 {
		t.Fatalf("queued clock counted: %v", got.Elapsed)
	}
	a := Scope{RootBudgetID: "root", MemberID: "run", SessionID: "parent", AttemptID: "run/1", Fence: 1}
	if err := l.BeginAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Second)
	if err := l.BindMember(ctx, MemberBinding{RootBudgetID: "root", MemberID: "child", SessionID: "child-session", OriginRunID: "run", ParentMemberID: "run", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	b := Scope{RootBudgetID: "root", MemberID: "child", SessionID: "child-session", AttemptID: "child/1", Fence: 1}
	if err := l.BeginAttempt(ctx, b); err != nil {
		t.Fatal(err)
	}
	clock.add(5 * time.Second)
	if err := l.EndAttempt(ctx, a, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	clock.add(5 * time.Second)
	if err := l.EndAttempt(ctx, b, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour)
	if got := snapshot(t, l, "root"); got.Elapsed != 12*time.Second {
		t.Fatalf("parallel/paused time counted incorrectly: %v", got.Elapsed)
	}
	a.AttemptID = "run/2"
	a.Fence = 2
	if err := l.BeginAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	clock.add(5 * time.Second)
	if err := l.EndAttempt(ctx, a, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, l, "root"); got.Elapsed != 17*time.Second {
		t.Fatalf("resume reset clock: %v", got.Elapsed)
	}
	clock.add(-time.Second)
	if _, err := l.Snapshot(ctx, "root"); !errors.Is(err, ErrClock) {
		t.Fatalf("backward clock accepted: %v", err)
	}
}

func TestHeldTemporaryCapacityFailureChargingAndProviderOverage(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{MaxTokens: 100})
	ctx := context.Background()
	first, err := l.ReserveModel(ctx, s, ModelRequest{OperationID: "first", Digest: "x", InputTokens: 10, MaxOutputTokens: 90})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.ReserveModel(ctx, s, ModelRequest{OperationID: "blocked", Digest: "x", InputTokens: 1, MaxOutputTokens: 1})
	var limit *LimitError
	if !errors.As(err, &limit) || !limit.Temporary {
		t.Fatalf("held quota treated as permanent exhaustion: %v", err)
	}
	if _, err = l.Settle(ctx, first, Settlement{Complete: true, Usage: harness.Usage{TotalTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	second, err := l.ReserveModel(ctx, s, ModelRequest{OperationID: "second", Digest: "x", InputTokens: 10, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := l.Settle(ctx, second, Settlement{Complete: false})
	if err != nil || usage.TotalTokens != 30 || !usage.Estimated {
		t.Fatalf("missing failed usage did not conservatively charge hold: %+v %v", usage, err)
	}
	third, err := l.ReserveModel(ctx, s, ModelRequest{OperationID: "overage", Digest: "x", InputTokens: 10, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.Settle(ctx, third, Settlement{Complete: true, Usage: harness.Usage{TotalTokens: 200}}); err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, l, "root")
	if got.SpentTokens != 240 || got.HeldTokens != 0 || got.BlockedReason != "tokens" {
		t.Fatalf("provider overage lost: %+v", got)
	}
	if _, err = l.ReserveTool(ctx, s, ToolRequest{OperationID: "after-overage", Digest: "x"}); !errors.As(err, &limit) {
		t.Fatalf("over-budget root admitted effect: %v", err)
	}
}

func TestNativeEndGateRejectsUnsettledReservations(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{MaxTokens: 100})
	ctx := context.Background()
	if _, err := l.ReserveModel(ctx, s, ModelRequest{OperationID: "inflight", Digest: "x", InputTokens: 10, MaxOutputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := l.EndAttempt(ctx, s, OutcomeWaitingInput); !errors.Is(err, ErrUnknown) {
		t.Fatalf("safe pause accepted unresolved effect: %v", err)
	}
	got := snapshot(t, l, "root")
	if got.HeldTokens != 30 {
		t.Fatalf("unsettled hold lost: %+v", got)
	}
}

func TestPersistenceCancellationAndTransactionFailureRemainVisible(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.ReserveTool(ctx, s, ToolRequest{OperationID: "canceled", Digest: "x"})
	var marker interface{ PersistenceFailure() bool }
	if !errors.Is(err, context.Canceled) || !errors.As(err, &marker) || !marker.PersistenceFailure() {
		t.Fatalf("SQL cancellation lost marker: %v", err)
	}
	if _, err = l.db.Exec("CREATE TRIGGER fixture_budget_fault BEFORE UPDATE ON budget_roots BEGIN SELECT RAISE(ABORT,'budget persistence fault'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.ReserveTool(context.Background(), s, ToolRequest{OperationID: "fault", Digest: "x"}); !errors.As(err, &marker) {
		t.Fatalf("SQL fault lost marker: %v", err)
	}
	var count int
	if err = l.db.QueryRow("SELECT count(*) FROM budget_reservations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial reservation committed: %d %v", count, err)
	}
	if _, err = l.db.Exec("DROP TRIGGER fixture_budget_fault"); err != nil {
		t.Fatal(err)
	}
}

func TestExternalFenceSharesReservationTransaction(t *testing.T) {
	l, _, s := budgetFixture(t, harness.BudgetLimits{})
	ctx := context.Background()
	if _, err := l.db.Exec("CREATE TABLE external_fence(value INTEGER NOT NULL); INSERT INTO external_fence VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	l.config.CheckEffectTx = func(ctx context.Context, tx *sql.Tx, scope Scope) error {
		var fence int64
		if err := tx.QueryRowContext(ctx, "SELECT value FROM external_fence").Scan(&fence); err != nil {
			return err
		}
		if fence != scope.Fence {
			return ErrScope
		}
		return nil
	}
	grant, err := l.ReserveTool(ctx, s, ToolRequest{OperationID: "reserved", Digest: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec("UPDATE external_fence SET value=2"); err != nil {
		t.Fatal(err)
	}
	if first, err := l.MarkDispatched(ctx, grant); first || !errors.Is(err, ErrScope) {
		t.Fatalf("stale dispatch passed external fence: %v %v", first, err)
	}
	if _, err = l.ReserveTool(ctx, s, ToolRequest{OperationID: "stale", Digest: "x"}); !errors.Is(err, ErrScope) {
		t.Fatalf("stale reservation: %v", err)
	}
	if got := snapshot(t, l, "root"); got.ToolCalls != 1 {
		t.Fatalf("rejected stale request charged quota: %+v", got)
	}
}

func TestHeartbeatCommitsExhaustedClockAndKeepsCleanupLease(t *testing.T) {
	l, clock, scope := budgetFixture(t, harness.BudgetLimits{Timeout: 5 * time.Second})
	ctx := context.Background()
	// Effect admission may reject cancellation, but heartbeat must continue
	// owning real cleanup until the final attempt transaction completes.
	l.config.CheckEffectTx = func(context.Context, *sql.Tx, Scope) error { return ErrScope }
	clock.add(10 * time.Second)
	err := l.Heartbeat(ctx, scope)
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Resource != "time" {
		t.Fatalf("heartbeat exhaustion signal: %v", err)
	}
	var lease int64
	if err = l.db.QueryRow("SELECT lease_ns FROM budget_attempts WHERE id=?", scope.AttemptID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if lease != clock.now().Add(time.Minute).UnixNano() {
		t.Fatal("limit signal rolled back live cleanup lease")
	}
	clock.add(50 * time.Second)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.HeartbeatTx(ctx, tx, scope); err != nil {
		tx.Rollback()
		t.Fatalf("transactional renewal rejected exhausted budget: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	clock.add(10 * time.Second)
	if err = l.EndAttempt(ctx, scope, OutcomeCancelled); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Hour)
	got := snapshot(t, l, "root")
	if got.Elapsed != 70*time.Second || got.BlockedReason != "time" {
		t.Fatalf("cleanup lifetime accounting: %+v", got)
	}
}

func TestExpiredAttemptCannotPublishSafeOutcomeAndEndedOutcomeIsImmutable(t *testing.T) {
	l, clock, scope := budgetFixture(t, harness.BudgetLimits{})
	ctx := context.Background()
	clock.add(2 * time.Minute)
	if err := l.EndAttempt(ctx, scope, OutcomeCompleted); !errors.Is(err, ErrUnknown) {
		t.Fatalf("expired attempt published safe terminal: %v", err)
	}
	if err := l.EndAttempt(ctx, scope, OutcomeInterrupted); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, l, "root"); got.BlockedReason != "unknown" {
		t.Fatalf("interrupted attempt not quarantined: %+v", got)
	}
	other, _, current := budgetFixture(t, harness.BudgetLimits{})
	if err := other.EndAttempt(ctx, current, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if err := other.EndAttempt(ctx, current, OutcomeCompleted); err != nil {
		t.Fatalf("same outcome is not idempotent: %v", err)
	}
	if err := other.EndAttempt(ctx, current, OutcomeFailed); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed attempt changed terminal outcome: %v", err)
	}
}
