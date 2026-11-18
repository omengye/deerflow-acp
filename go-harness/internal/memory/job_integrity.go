package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

func sourceIntegrityError(err error) bool {
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrSourceSnapshotMissing) || errors.Is(err, ErrSourcePolicyMissing) ||
		errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, harness.ErrPermissionDenied) || errors.As(err, &syntax) || errors.As(err, &shape)
}

// Use the selected database row and ledger binding, never a corrupt payload's
// identity. Response settlement precedes disposition; unknown spend keeps holds.
func (s *Store) discardCorruptJobTx(ctx context.Context, tx *sql.Tx, id string) error {
	j := Job{ID: id, MemberID: jobMember(id)}
	if err := tx.QueryRowContext(ctx, "SELECT run_id,input_id FROM memory_jobs WHERE id=?", id).Scan(&j.RunID, &j.InputID); err != nil {
		return err
	}
	err := tx.QueryRowContext(ctx, "SELECT root_id,session_id FROM budget_members WHERE id=? AND kind='memory_job'", j.MemberID).Scan(&j.RootBudgetID, &j.SessionID)
	if err != nil {
		return err
	}
	l, err := readJobLease(ctx, tx, id)
	if err != nil {
		return err
	}
	if s.ledger == nil {
		return budget.ErrScope
	}
	unknown, err := s.settleJobAttemptTx(ctx, tx, j, l)
	if err != nil {
		return err
	}
	status, reason, usage := "discarded", "source_integrity", "no_active_call"
	if l.attempt != "" {
		usage = "settled"
	}
	if unknown {
		status, reason, usage = "blocked", "budget_unknown", "unknown"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_job_dispositions(job_id,reason,usage_state) VALUES(?,'source_integrity',?)
 ON CONFLICT(job_id) DO NOTHING`, id, usage); err != nil {
		return err
	}
	return finishJobStateTx(ctx, tx, j, status, reason, status, time.Now())
}
