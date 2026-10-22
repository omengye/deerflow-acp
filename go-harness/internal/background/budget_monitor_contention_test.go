package background

import (
	"context"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBudgetMonitorConnectionContentionDoesNotCancelLiveAttempt(t *testing.T) {
	s, _, adapter := realLedgerFixture(t, harness.BudgetLimits{}, nil, nil)
	task := submitFixture(t, s, submission("parent", "contended-monitor"))
	_, attemptCtx, state := claimFixture(t, s, task.ID)
	if err := adapter.BeforeAttempt(attemptCtx, state.scope); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	connection, err := s.store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	run, stop := startBudgetMonitor(ctx, adapter, state.scope, 20*time.Millisecond)
	defer func() {
		connection.Close()
		if err := stop(); err != nil {
			t.Errorf("monitor failed during connection contention: %v", err)
		}
	}()
	// Hold the sole connection beyond a probe's deadline but within both
	// confirmed leases, as a large checkpoint transaction can do on CI.
	select {
	case <-run.Done():
		t.Fatal("connection queue time was treated as a failed budget validation")
	case <-time.After(6 * time.Second):
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CheckAttemptBudget(ctx, state.scope); err != nil {
		t.Fatalf("attempt lost its valid budget or lease: %v", err)
	}
}
