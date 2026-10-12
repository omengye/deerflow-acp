package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestStoppedUncertainReceiptOnlyUnblocksNewRuns(t *testing.T) {
	for _, reason := range []string{"cancelled", "failed", "interrupted", "end_turn"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			s, req := receiptFixture(t)
			appendReceiptEvent(t, s, req, "tool_start", "old-effect", "execute")
			appendReceiptEvent(t, s, req, "tool_execute", "old-effect", "execute")
			if err := s.Store.Finish(ctx, req.RunID, reason, nil); err != nil {
				t.Fatal(err)
			}
			err := s.Store.requireReconciled(ctx, req.Session.ID)
			allowed := reason == "cancelled"
			if (err == nil) != allowed || !allowed && !errors.Is(err, harness.ErrReconciliationRequired) {
				t.Fatalf("new-run admission for %s: %v", reason, err)
			}
			marked, err := stoppedToolRecoveryContext(ctx, s.Store.db, req.Session.ID)
			if err != nil || interaction.HasStoppedToolRecovery(marked) != allowed {
				t.Fatalf("recovery context for %s: %v", reason, err)
			}
			for _, runID := range []string{req.RunID, "new-run"} {
				err := executionTx(ctx, s.Store, func(tx *sql.Tx) error {
					_, err := executionReceiptFrontier(ctx, tx, req.Session.ID, runID)
					return err
				})
				wantBlocked := !allowed || runID == req.RunID
				if errors.Is(err, harness.ErrReconciliationRequired) != wantBlocked {
					t.Fatalf("checkpoint frontier for %s/%s: %v", reason, runID, err)
				}
			}
			receipts, err := s.Store.ListToolReceipts(ctx, req.Session.ID)
			if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || receipts[0].Review != nil {
				t.Fatalf("stopped uncertainty lost: %+v %v", receipts, err)
			}
			if allowed {
				// One stopped run must not waive an unrelated unresolved failure.
				req.RunID, req.InputID = "other-run", "other-input"
				if err := s.Store.BeginRun(ctx, req); err != nil {
					t.Fatal(err)
				}
				appendReceiptEvent(t, s, req, "tool_start", "other-effect", "execute")
				appendReceiptEvent(t, s, req, "tool_execute", "other-effect", "execute")
				if err := s.Store.Finish(ctx, req.RunID, "end_turn", errors.New("unconfirmed failure")); err != nil {
					t.Fatal(err)
				}
				if err := s.Store.requireReconciled(ctx, req.Session.ID); !errors.Is(err, harness.ErrReconciliationRequired) {
					t.Fatalf("unrelated uncertainty bypassed: %v", err)
				}
			}
		})
	}
}
