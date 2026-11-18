package memory

import (
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
)

func TestMemoryReviewB6BadHeadDoesNotStarve(t *testing.T) {
	for _, fault := range []string{"missing_input", "bad_digest", "bad_event", "bad_event_json", "bad_manifest_json", "bad_payload_json", "bad_payload_id", "bad_payload_run", "legacy_snapshot", "saved_result", "unknown_spend", "saved_payload_id", "unknown_payload_id"} {
		t.Run(fault, func(t *testing.T) {
			s, scope := personalStore(t)
			bad := stage1AcceptedJob(t, s, scope, "bad", "input-bad", "我在北京")
			paid := fault == "saved_result" || fault == "saved_payload_id"
			unknown := fault == "unknown_spend" || fault == "unknown_payload_id"
			if paid || unknown {
				claimed, err := s.ClaimJob(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				g, err := s.ReserveJobCall(t.Context(), claimed, "bad-head", 10, 100)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.DispatchJobCall(t.Context(), claimed, g); err != nil {
					t.Fatal(err)
				}
				if paid {
					if err = s.SaveJobResponse(t.Context(), claimed, JobResponse{Raw: stage1City(t), Complete: true, Usage: harness.Usage{TotalTokens: 15}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.db.Exec("UPDATE memory_job_leases SET lease_ns=0 WHERE job_id=?", bad.ID); err != nil {
					t.Fatal(err)
				}
			}
			good := stage1AcceptedJob(t, s, scope, "good", "input-good", "我在上海")
			statement := "DELETE FROM harness_inputs WHERE id=?"
			args := []any{bad.InputID}
			switch fault {
			case "bad_digest":
				statement = "UPDATE memory_inputs SET manifest=json_set(CAST(manifest AS TEXT),'$.input_sha','bad') WHERE input_id=?"
			case "bad_event":
				statement = "UPDATE harness_events SET event=json_set(CAST(event AS TEXT),'$.kind','tool_end') WHERE run_id=?"
				args = []any{bad.RunID}
			case "bad_event_json":
				statement = "UPDATE harness_events SET event='{' WHERE run_id=?"
				args = []any{bad.RunID}
			case "bad_manifest_json":
				statement = "UPDATE memory_inputs SET manifest='{' WHERE input_id=?"
			case "bad_payload_json":
				statement = "UPDATE memory_jobs SET payload='{' WHERE id=?"
				args = []any{bad.ID}
			case "bad_payload_id", "saved_payload_id", "unknown_payload_id":
				statement = "UPDATE memory_jobs SET payload=json_set(CAST(payload AS TEXT),'$.id',?) WHERE id=?"
				args = []any{good.ID, bad.ID}
			case "bad_payload_run":
				statement = "UPDATE memory_jobs SET payload=json_set(CAST(payload AS TEXT),'$.run_id',?) WHERE id=?"
				args = []any{good.RunID, bad.ID}
			case "legacy_snapshot":
				statement = "DELETE FROM memory_inputs WHERE input_id=?"
			}
			if _, err := s.db.Exec(statement, args...); err != nil {
				t.Fatal(err)
			}
			var claimed Job
			for n := 0; n < 3; n++ {
				j, err := s.ClaimJob(t.Context())
				if err != nil && !errors.Is(err, harness.ErrNotFound) {
					t.Fatal(err)
				}
				if j.ID != "" {
					claimed = j
					break
				}
			}
			if claimed.ID != good.ID {
				t.Fatalf("healthy successor not claimed: %q", claimed.ID)
			}
			var status, reason string
			if err := s.db.QueryRow("SELECT status,error_code FROM memory_jobs WHERE id=?", bad.ID).Scan(&status, &reason); err != nil {
				t.Fatal(err)
			}
			if unknown {
				snapshot, err := s.ledger.Snapshot(t.Context(), bad.RootBudgetID)
				if err != nil || status != "blocked" || snapshot.HeldTokens == 0 || snapshot.BlockedReason != "unknown" {
					t.Fatalf("lost unknown hold: %s %+v %v", status, snapshot, err)
				}
			} else if status != "discarded" {
				t.Fatalf("bad job=%s/%s", status, reason)
			}
			if fault == "bad_manifest_json" && reason != "source_integrity" {
				t.Fatalf("malformed source misclassified: %s", reason)
			}
			if paid {
				snapshot, err := s.ledger.Snapshot(t.Context(), bad.RootBudgetID)
				var saved bool
				var raw string
				if e := s.db.QueryRow("SELECT response_saved,response FROM memory_job_leases WHERE job_id=?", bad.ID).Scan(&saved, &raw); e != nil {
					t.Fatal(e)
				}
				if err != nil || snapshot.SpentTokens != 15 || snapshot.HeldTokens != 0 || !saved || raw != stage1City(t) {
					t.Fatalf("lost paid response: %+v %v", snapshot, err)
				}
			}
		})
	}
}

func TestMemoryReviewB6DatabaseFailureIsNotDisposition(t *testing.T) {
	s, scope := personalStore(t)
	j := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	if _, err := s.db.Exec("ALTER TABLE harness_inputs RENAME TO temporarily_unavailable_inputs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(t.Context()); err == nil || errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("database failure hidden: %v", err)
	}
	var state string
	if err := s.db.QueryRow("SELECT status FROM memory_jobs WHERE id=?", j.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("database failure discarded job: %s %v", state, err)
	}
	if _, err := s.db.Exec("ALTER TABLE temporarily_unavailable_inputs RENAME TO harness_inputs"); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimJob(t.Context())
	if err != nil || claimed.ID != j.ID {
		t.Fatalf("cannot recover after database repair: %s %v", claimed.ID, err)
	}
}
