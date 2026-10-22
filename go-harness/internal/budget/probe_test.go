package budget

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestProbeDefersOnlyBeforeAcquiringConnection(t *testing.T) {
	ledger, _, scope := budgetFixture(t, harness.BudgetLimits{})
	conn, err := ledger.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if checked, err := ledger.Probe(ctx, scope); checked || err != nil {
		t.Fatalf("busy pool was treated as a completed/failed check: checked=%v error=%v", checked, err)
	}
	if err := ledger.Validate(ctx, scope); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("effect admission no longer fails closed: %v", err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if checked, err := ledger.Probe(t.Context(), scope); !checked || err != nil {
		t.Fatalf("validation did not resume after contention: checked=%v error=%v", checked, err)
	}
}

func TestProbeRetainsTransactionFailuresAndFences(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, context.Canceled, errors.New("unavailable storage")} {
		t.Run(failure.Error(), func(t *testing.T) {
			ledger, _, scope := budgetFixture(t, harness.BudgetLimits{})
			ledger.config.CheckEffectTx = func(context.Context, *sql.Tx, Scope) error { return failure }
			checked, err := ledger.Probe(t.Context(), scope)
			var persistence *PersistenceError
			if !checked || !errors.As(err, &persistence) || !errors.Is(err, failure) {
				t.Fatalf("transaction failure was hidden: checked=%v error=%v", checked, err)
			}
		})
	}
	ledger, clock, scope := budgetFixture(t, harness.BudgetLimits{Timeout: time.Second})
	clock.add(2 * time.Second)
	checked, err := ledger.Probe(t.Context(), scope)
	var limit *LimitError
	if !checked || !errors.As(err, &limit) || limit.Resource != "time" {
		t.Fatalf("time limit was hidden: checked=%v error=%v", checked, err)
	}
	ledger, clock, scope = budgetFixture(t, harness.BudgetLimits{})
	clock.add(2 * time.Minute)
	if checked, err = ledger.Probe(t.Context(), scope); !checked || err == nil {
		t.Fatalf("expired lease was accepted: checked=%v error=%v", checked, err)
	}
	ledger, _, scope = budgetFixture(t, harness.BudgetLimits{})
	scope.Fence++
	if checked, err = ledger.Probe(t.Context(), scope); !checked || !errors.Is(err, ErrScope) {
		t.Fatalf("forged attempt was accepted: checked=%v error=%v", checked, err)
	}
}

func TestProbeDoesNotHideCancellationOrClosedDatabase(t *testing.T) {
	for _, mode := range []string{"canceled", "closed"} {
		t.Run(mode, func(t *testing.T) {
			ledger, _, scope := budgetFixture(t, harness.BudgetLimits{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "closed" {
				if err := ledger.db.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			checked, err := ledger.Probe(ctx, scope)
			var persistence *PersistenceError
			if checked || !errors.As(err, &persistence) {
				t.Fatalf("connection error was deferred: checked=%v error=%v", checked, err)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause was lost: %v", err)
			}
		})
	}
}

func TestProbeCancellationAfterAcquisitionFailsClosed(t *testing.T) {
	ledger, _, scope := budgetFixture(t, harness.BudgetLimits{})
	readCounters := func() (revision, calls int64) {
		t.Helper()
		if err := ledger.db.QueryRowContext(t.Context(), "SELECT revision,model_calls FROM budget_roots WHERE id=?", scope.RootBudgetID).Scan(&revision, &calls); err != nil {
			t.Fatal(err)
		}
		return
	}
	beforeRevision, beforeCalls := readCounters()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ledger.config.CheckEffectTx = func(ctx context.Context, tx *sql.Tx, _ Scope) error {
		// Force a rollback after a successful SQL statement, not a pool timeout.
		if _, err := tx.ExecContext(ctx, "UPDATE budget_roots SET model_calls=42"); err != nil {
			return err
		}
		cancel()
		return nil
	}
	checked, err := ledger.Probe(ctx, scope)
	var persistence *PersistenceError
	if !checked || !errors.As(err, &persistence) || !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction cancellation was deferred: checked=%v error=%v", checked, err)
	}
	afterRevision, afterCalls := readCounters()
	if afterRevision != beforeRevision || afterCalls != beforeCalls {
		t.Fatalf("failed probe committed partial accounting: revision=%d->%d calls=%d->%d", beforeRevision, afterRevision, beforeCalls, afterCalls)
	}
}
