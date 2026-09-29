package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const protectedToolDigest = "write_file:{path:a.txt,content:approved}"

func nextToolAttempt(t *testing.T, l *Ledger, old Scope) Scope {
	t.Helper()
	next := old
	next.Fence++
	next.AttemptID = fmt.Sprintf("%s/%d", old.MemberID, next.Fence)
	if err := l.BeginAttempt(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	return next
}
func pauseFixtureTool(t *testing.T, l *Ledger, scope Scope, id string) Reservation {
	t.Helper()
	grant, err := l.ReserveTool(context.Background(), scope, ToolRequest{OperationID: id, Digest: protectedToolDigest})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.PauseTool(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestToolContinuationWithOneCallLimit(t *testing.T) {
	l, _, original := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
	ctx := context.Background()
	grant := pauseFixtureTool(t, l, original, "original")
	if err := l.PauseTool(ctx, grant); err != nil {
		t.Fatalf("pause replay: %v", err)
	}
	if first, _ := l.MarkDispatched(ctx, grant); first {
		t.Fatal("paused grant authorized external I/O")
	}
	if err := l.EndAttempt(ctx, original, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	current := nextToolAttempt(t, l, original)
	request := ToolRequest{OperationID: "resumed", Digest: protectedToolDigest}
	resumed, err := l.ResumeTool(ctx, current, request, grant.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Scope != current || resumed.Kind != "tool" || resumed.State != "reserved" || resumed.HeldTokens != 0 {
		t.Fatalf("bad continuation grant: %+v", resumed)
	}
	replay, err := l.ResumeTool(ctx, current, request, grant.OperationID)
	if err != nil || replay != resumed {
		t.Fatalf("continuation replay: %+v %v", replay, err)
	}
	if first, err := l.MarkDispatched(ctx, resumed); err != nil || !first {
		t.Fatalf("resumed dispatch: %v %v", first, err)
	}
	if first, err := l.MarkDispatched(ctx, resumed); err != nil || first {
		t.Fatalf("resumed dispatch replay: %v %v", first, err)
	}
	if _, err = l.Settle(ctx, resumed, Settlement{Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err = l.EndAttempt(ctx, current, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if replay, err = l.ResumeTool(ctx, current, request, grant.OperationID); err != nil || replay.State != "settled" {
		t.Fatalf("settled continuation replay: %+v %v", replay, err)
	}
	if first, _ := l.MarkDispatched(ctx, replay); first {
		t.Fatal("settled continuation replay authorized I/O")
	}
	if got := snapshot(t, l, "root"); got.ToolCalls != 1 || got.HeldTokens != 0 {
		t.Fatalf("approval used another tool call: %+v", got)
	}
}

func TestToolContinuationRejectsMismatchedScopeDigestAndPredecessor(t *testing.T) {
	l, _, original := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
	ctx := context.Background()
	parent := pauseFixtureTool(t, l, original, "original")
	if err := l.EndAttempt(ctx, original, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	current := nextToolAttempt(t, l, original)
	for _, mutate := range []func(*Scope, *ToolRequest){
		func(s *Scope, _ *ToolRequest) { s.RootBudgetID = "other" },
		func(s *Scope, _ *ToolRequest) { s.MemberID = "other" },
		func(s *Scope, _ *ToolRequest) { s.SessionID = "other" },
		func(s *Scope, _ *ToolRequest) { s.Fence++ },
		func(_ *Scope, r *ToolRequest) { r.Digest = "different tool or arguments" },
	} {
		scope := current
		request := ToolRequest{OperationID: "child", Digest: protectedToolDigest}
		mutate(&scope, &request)
		if _, err := l.ResumeTool(ctx, scope, request, parent.OperationID); err == nil {
			t.Fatalf("mismatch accepted scope=%+v request=%+v", scope, request)
		}
	}
	request := ToolRequest{OperationID: "child", Digest: protectedToolDigest}
	if _, err := l.ResumeTool(ctx, current, request, "missing"); err == nil {
		t.Fatal("missing parent accepted")
	}
	if _, err := l.ResumeTool(ctx, current, request, parent.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ResumeTool(ctx, current, request, "different-parent"); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing child rebound to another parent: %v", err)
	}
	if _, err := l.ResumeTool(ctx, current, ToolRequest{OperationID: "another-child", Digest: protectedToolDigest}, parent.OperationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("parent consumed twice: %v", err)
	}
	if err := l.PauseTool(ctx, parent); err == nil {
		t.Fatal("consumed parent restored to paused")
	}
	if _, err := l.ReserveTool(ctx, current, ToolRequest{OperationID: "fresh-tool", Digest: protectedToolDigest}); err == nil {
		t.Fatal("continuation refunded original tool count")
	}
}

func TestToolContinuationRequiresWaitingInputOutcome(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeCancelled, OutcomeFailed, OutcomeCompleted} {
		t.Run(string(outcome), func(t *testing.T) {
			l, _, original := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
			ctx := context.Background()
			grant := pauseFixtureTool(t, l, original, "original")
			if err := l.EndAttempt(ctx, original, outcome); err != nil {
				t.Fatal(err)
			}
			current := nextToolAttempt(t, l, original)
			if _, err := l.ResumeTool(ctx, current, ToolRequest{OperationID: "resumed", Digest: protectedToolDigest}, grant.OperationID); !errors.Is(err, ErrScope) {
				t.Fatalf("non-waiting predecessor continued: %v", err)
			}
		})
	}
}

func TestToolContinuationRollbackPreservesPausedAdmission(t *testing.T) {
	l, _, original := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
	ctx := context.Background()
	parent := pauseFixtureTool(t, l, original, "original")
	if err := l.EndAttempt(ctx, original, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	current := nextToolAttempt(t, l, original)
	if _, err := l.db.Exec("CREATE TRIGGER continuation_fault BEFORE UPDATE ON budget_roots BEGIN SELECT RAISE(ABORT,'continuation persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	request := ToolRequest{OperationID: "child", Digest: protectedToolDigest}
	_, err := l.ResumeTool(ctx, current, request, parent.OperationID)
	var persistence *PersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("fault lost persistence marker: %v", err)
	}
	var state string
	var count int
	if err = l.db.QueryRow("SELECT state FROM budget_reservations WHERE id=?", parent.OperationID).Scan(&state); err != nil || state != "paused" {
		t.Fatalf("rollback lost parent: %s %v", state, err)
	}
	if err = l.db.QueryRow("SELECT count(*) FROM budget_tool_continuations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial continuation link: %d %v", count, err)
	}
	if err = l.db.QueryRow("SELECT count(*) FROM budget_reservations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial child reservation: %d %v", count, err)
	}
	if _, err = l.db.Exec("DROP TRIGGER continuation_fault"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.ResumeTool(ctx, current, request, parent.OperationID); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, l, "root"); got.ToolCalls != 1 {
		t.Fatalf("retry charged twice: %+v", got)
	}
}

func TestToolContinuationConcurrentConsumersAndReplay(t *testing.T) {
	clock := newClock()
	path := filepath.Join(t.TempDir(), "continuation.db")
	ctx := context.Background()
	a := openBudget(t, path, Config{Now: clock.now})
	b := openBudget(t, path, Config{Now: clock.now})
	if err := a.CreateRoot(ctx, RootSpec{RootBudgetID: "root", RootRunID: "run", SessionID: "parent", Limits: harness.BudgetLimits{MaxToolCalls: 1}}); err != nil {
		t.Fatal(err)
	}
	original := Scope{RootBudgetID: "root", MemberID: "run", SessionID: "parent", AttemptID: "run/1", Fence: 1}
	if err := a.BeginAttempt(ctx, original); err != nil {
		t.Fatal(err)
	}
	parent := pauseFixtureTool(t, a, original, "original")
	if err := a.EndAttempt(ctx, original, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	current := nextToolAttempt(t, a, original)
	var wg sync.WaitGroup
	success := make(chan Reservation, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ledger := a
			if i%2 == 0 {
				ledger = b
			}
			grant, err := ledger.ResumeTool(ctx, current, ToolRequest{OperationID: fmt.Sprintf("contender-%d", i), Digest: protectedToolDigest}, parent.OperationID)
			if err == nil {
				success <- grant
			} else if !errors.Is(err, ErrConflict) {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(success)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var grants []Reservation
	for grant := range success {
		grants = append(grants, grant)
	}
	if len(grants) != 1 {
		t.Fatalf("one paused admission produced %d successors", len(grants))
	}
	winner := grants[0]
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.ResumeTool(ctx, current, ToolRequest{OperationID: winner.OperationID, Digest: protectedToolDigest}, parent.OperationID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := snapshot(t, a, "root"); got.ToolCalls != 1 {
		t.Fatalf("concurrent continuation changed quota: %+v", got)
	}
}

func TestRepeatedToolPauseContinuationChainKeepsSingleAdmission(t *testing.T) {
	l, _, scope := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
	ctx := context.Background()
	grant := pauseFixtureTool(t, l, scope, "tool/1")
	for i := 2; i <= 4; i++ {
		if err := l.EndAttempt(ctx, scope, OutcomeWaitingInput); err != nil {
			t.Fatal(err)
		}
		scope = nextToolAttempt(t, l, scope)
		var err error
		grant, err = l.ResumeTool(ctx, scope, ToolRequest{OperationID: fmt.Sprintf("tool/%d", i), Digest: protectedToolDigest}, grant.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if i < 4 {
			if err = l.PauseTool(ctx, grant); err != nil {
				t.Fatal(err)
			}
		}
	}
	if first, err := l.MarkDispatched(ctx, grant); err != nil || !first {
		t.Fatalf("final chain dispatch: %v %v", first, err)
	}
	if _, err := l.Settle(ctx, grant, Settlement{Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.EndAttempt(ctx, scope, OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, l, "root"); got.ToolCalls != 1 {
		t.Fatalf("approval loop increased quota: %+v", got)
	}
}

func TestPauseToolRejectsDispatchedModelAndUnownedGrants(t *testing.T) {
	l, _, scope := budgetFixture(t, harness.BudgetLimits{})
	ctx := context.Background()
	tool, err := l.ReserveTool(ctx, scope, ToolRequest{OperationID: "tool", Digest: protectedToolDigest})
	if err != nil {
		t.Fatal(err)
	}
	wrong := tool
	wrong.Scope.SessionID = "other"
	if err = l.PauseTool(ctx, wrong); !errors.Is(err, ErrScope) {
		t.Fatalf("unowned grant paused: %v", err)
	}
	if _, err = l.MarkDispatched(ctx, tool); err != nil {
		t.Fatal(err)
	}
	if err = l.PauseTool(ctx, tool); !errors.Is(err, ErrConflict) {
		t.Fatalf("dispatched tool paused: %v", err)
	}
	model, err := l.ReserveModel(ctx, scope, ModelRequest{OperationID: "model", Digest: "input", InputTokens: 1, MaxOutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.PauseTool(ctx, model); !errors.Is(err, ErrScope) {
		t.Fatalf("model reservation paused: %v", err)
	}
}

func TestToolContinuationChecksExternalFenceBeforeConsumingParent(t *testing.T) {
	l, _, original := budgetFixture(t, harness.BudgetLimits{MaxToolCalls: 1})
	ctx := context.Background()
	parent := pauseFixtureTool(t, l, original, "original")
	if err := l.EndAttempt(ctx, original, OutcomeWaitingInput); err != nil {
		t.Fatal(err)
	}
	current := nextToolAttempt(t, l, original)
	var checked bool
	l.config.CheckEffectTx = func(_ context.Context, tx *sql.Tx, scope Scope) error {
		checked = true
		if scope != current {
			return ErrConflict
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM budget_tool_continuations").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrConflict
		}
		return ErrScope
	}
	if _, err := l.ResumeTool(ctx, current, ToolRequest{OperationID: "child", Digest: protectedToolDigest}, parent.OperationID); !errors.Is(err, ErrScope) || !checked {
		t.Fatalf("external fence was bypassed: checked=%v err=%v", checked, err)
	}
	var state string
	if err := l.db.QueryRow("SELECT state FROM budget_reservations WHERE id=?", parent.OperationID).Scan(&state); err != nil || state != "paused" {
		t.Fatalf("fence rejection consumed parent: %s %v", state, err)
	}
}
