package memory

import (
	"database/sql"
	"encoding/json"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
)

func TestMemoryReviewB5ExposureLabelsPreserved(t *testing.T) {
	for _, kind := range []string{"fresh", "existing_fact", "earlier_turn", "tool_receipt"} {
		t.Run(kind, func(t *testing.T) {
			s, scope := personalStore(t)
			if kind == "existing_fact" {
				_, err := s.Create(t.Context(), scope, Candidate{Content: "用户最近告知所在地是北京。", Category: "profile", Confidence: 1, Source: Source{ID: "seed", Kind: "operator"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			job := stage1AcceptedJob(t, s, scope, "first", "first-input", "我叫张三")
			if kind == "earlier_turn" {
				req := harness.RunRequest{RunID: "second", InputID: "second-input", RootBudgetID: "second", Session: harness.Session{ID: job.SessionID, CWD: job.Workspace}, Input: []harness.Content{{Type: "text", Text: "记住我叫张三"}}}
				raw, _ := json.Marshal(req.Input)
				event, _ := json.Marshal(harness.RunEvent{RunID: req.RunID, SessionID: req.Session.ID, Kind: "user_message", Content: req.Input})
				err := transact(t.Context(), s.db, func(tx *sql.Tx) error {
					if _, err := tx.Exec("INSERT INTO harness_runs VALUES(?,?,?,'completed')", req.RunID, req.InputID, req.Session.ID); err != nil {
						return err
					}
					if _, err := tx.Exec("INSERT INTO harness_inputs VALUES(?,?,?)", req.InputID, req.RunID, raw); err != nil {
						return err
					}
					row, err := tx.Exec("INSERT INTO harness_events(run_id,event) VALUES(?,?)", req.RunID, event)
					if err != nil {
						return err
					}
					seq, err := row.LastInsertId()
					if err != nil {
						return err
					}
					return s.CaptureInputTx(t.Context(), tx, req, seq, raw, scope.subject, []Scope{scope}, true, true)
				})
				if err != nil {
					t.Fatal(err)
				}
				job.RunID, job.InputID = req.RunID, req.InputID
			}
			m, err := s.InputSnapshot(t.Context(), job.RunID, job.InputID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "tool_receipt" {
				if _, err = s.db.Exec("INSERT INTO harness_tool_receipts(session_id,receipt) VALUES(?,'{}')", job.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.ContextProvenance(t.Context(), m)
			if err != nil {
				t.Fatal(err)
			}
			want := ExternalContextProvenance
			if kind == "fresh" {
				want = UserSupportedProvenance
			}
			t.Logf("provenance=%s", got)
			if got != want {
				t.Fatalf("got=%s want=%s", got, want)
			}
		})
	}
}
