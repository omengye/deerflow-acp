package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func stage3AttachLedger(t *testing.T, s *Store) {
	t.Helper()
	l, err := budget.New(s.db, budget.Config{CheckEffectTx: s.CheckJobBudgetEffectTx, CheckHeartbeatTx: s.CheckJobBudgetHeartbeatTx})
	if err != nil {
		t.Fatal(err)
	}
	s.SetJobLedger(l)
}
func stage3SaveFixture(t *testing.T, s *Store, j Job, raw string) error {
	t.Helper()
	g, err := s.ReserveJobCall(t.Context(), j, "fixture", 10, 100)
	if err != nil {
		return err
	}
	if first, e := s.DispatchJobCall(t.Context(), j, g); e != nil {
		return e
	} else if !first {
		return budget.ErrUnknown
	}
	if err = s.SaveJobResponse(t.Context(), j, JobResponse{Raw: raw, Complete: true, Usage: harness.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}); err != nil {
		return err
	}
	return s.SettleJobResponse(t.Context(), j)
}

// Prior source/delete tests now use the same metered receipt gate as the
// worker. Existing final-state, ordinal and atomicity assertions remain intact.
func stage3ApplyFixture(t *testing.T, s *Store, ctx context.Context, j Job, raw string) error {
	t.Helper()
	var status, persisted string
	if err := s.db.QueryRowContext(ctx, "SELECT status,result FROM memory_jobs WHERE id=?", j.ID).Scan(&status, &persisted); err != nil {
		return err
	}
	if status == "completed" {
		return s.ApplyJob(ctx, j, raw)
	}
	if status == "discarded" {
		return s.ApplyJob(ctx, j, raw)
	}
	if status == "queued" {
		claimed, err := s.ClaimJob(ctx)
		if errors.Is(err, harness.ErrNotFound) {
			var reason string
			if e := s.db.QueryRowContext(ctx, "SELECT status,error_code FROM memory_jobs WHERE id=?", j.ID).Scan(&status, &reason); e != nil {
				return e
			}
			if status == "discarded" && reason == "superseded" || status == "completed" && reason == "explicit_action" {
				return nil
			}
		}
		if err != nil {
			return err
		}
		if claimed.ID != j.ID {
			return budget.ErrConflict
		}
		// Preserve deliberately forged source/version fields. Only lease identity
		// comes from the host; ApplyJob must still re-read immutable source data.
		j.ClaimToken, j.WorkerAttemptID, j.OperationID, j.Fence = claimed.ClaimToken, claimed.WorkerAttemptID, claimed.OperationID, claimed.Fence
		j.MemberID = claimed.MemberID
	}
	if j.ClaimToken == "" {
		if err := transact(ctx, s.db, func(tx *sql.Tx) error {
			l, err := readJobLease(ctx, tx, j.ID)
			if err != nil {
				return err
			}
			j.ClaimToken, j.WorkerAttemptID, j.OperationID, j.Fence = l.token, l.attempt, l.operation, l.fence
			return nil
		}); err != nil {
			return err
		}
	}
	if persisted == "" {
		if err := stage3SaveFixture(t, s, j, raw); err != nil {
			return err
		}
	}
	return s.ApplyJob(ctx, j, raw)
}
