package memory

import (
	"database/sql"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
)

func TestMemoryReviewB1PreservesFailuresAndResumes(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		state          harness.ExecutionStatus
		skipped        bool
		want           string
	}{
		{"not_invoked", "pending", harness.ExecutionCompleted, true, "postrun_skipped"},
		{"unknown_origin", "pending", harness.ExecutionCompleted, false, "pending"},
		{"started", "started", harness.ExecutionCompleted, true, "started"},
		{"failed", "model_error", harness.ExecutionCompleted, true, "model_error"},
		{"waiting", "pending", harness.ExecutionWaitingInput, true, "pending"},
		{"cancelled", "pending", harness.ExecutionCancelled, true, "discarded"},
		{"failed_run", "pending", harness.ExecutionFailed, true, "discarded"},
		{"saved_response", "pending", harness.ExecutionCompleted, true, "pending"},
		{"audit_started", "pending", harness.ExecutionCompleted, true, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scope := personalStore(t)
			j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			if _, err := s.db.Exec("DELETE FROM memory_job_leases; DELETE FROM memory_jobs; UPDATE memory_inputs SET job_id='',decision=?", tc.decision); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "saved_response":
				if err := s.SaveExtractionResult(t.Context(), j.RunID, j.InputID, stage1City(t), UserSupportedProvenance); err != nil {
					t.Fatal(err)
				}
			case "audit_started":
				if err := s.AuditExtraction(t.Context(), j.RunID, j.InputID, "attempt", "started", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("UPDATE memory_inputs SET decision=?", tc.decision); err != nil {
				t.Fatal(err)
			}
			if err := transact(t.Context(), s.db, func(tx *sql.Tx) error { return s.SettleInputDecisionTx(t.Context(), tx, j.RunID, tc.state, tc.skipped) }); err != nil {
				t.Fatal(err)
			}
			got, err := s.InputDecision(t.Context(), j.RunID, j.InputID)
			if err != nil || got != tc.want {
				t.Fatalf("decision=%s want=%s %v", got, tc.want, err)
			}
		})
	}
}
