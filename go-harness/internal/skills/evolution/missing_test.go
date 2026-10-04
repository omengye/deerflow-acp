package evolution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func missingRequest(root harness.RunRequest, runID, key string) harness.SkillMissingReportRequest {
	return harness.SkillMissingReportRequest{SessionID: root.Session.ID, RunID: runID, RequestKey: key, Kind: "missing_skill", Summary: "A repeatable verification workflow is missing."}
}

func mustSubmitMissing(t *testing.T, s *Service, r harness.SkillMissingReportRequest) harness.SkillMissingReportResult {
	t.Helper()
	result, err := s.SubmitMissingReport(context.Background(), r)
	if err != nil {
		t.Fatalf("missing report: %+v %v", result, err)
	}
	return result
}

func missingSecondRoot(t *testing.T, f *collectorFixture, session harness.Session) harness.RunRequest {
	t.Helper()
	ctx := context.Background()
	run := harness.RunRequest{Session: session, RunID: hr.NewID(), InputID: hr.NewID()}
	run.RootBudgetID = run.RunID
	if err := f.runtime.BeginRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := f.ledger.CreateRoot(ctx, budget.RootSpec{RootBudgetID: run.RootBudgetID, RootRunID: run.RunID, SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.Finish(ctx, run.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestMissingReportsNeedNoSkillReadsAndDeduplicateDescendants(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	child := f.run(t, root.RootBudgetID, 0, "failed", false)
	beforeHead := f.head(t)
	initial, err := f.service.MissingReport(ctx, root.Session.ID, child.RunID)
	if err != nil || initial.RootID != root.RootBudgetID || initial.RootSessionID != root.Session.ID || initial.Report != nil {
		t.Fatalf("empty report: %+v %v", initial, err)
	}
	request := missingRequest(root, root.RunID, "first")
	first := mustSubmitMissing(t, f.service, request)
	if first.CurrentRevision != 1 || first.Replayed || first.Report.RootID != root.RootBudgetID || first.Report.Origin != "operator" || first.Report.Attribution != "unassessed" {
		t.Fatalf("report identity: %+v", first)
	}
	request.RunID, request.ExpectedReportVersion, request.RequestKey = child.RunID, 1, "child"
	second := mustSubmitMissing(t, f.service, request)
	if second.Report.ID != first.Report.ID || second.CurrentRevision != 2 || second.Report.RunID != child.RunID {
		t.Fatalf("descendant became a new vote: %+v", second)
	}
	page, err := f.service.QueryMissingReports(ctx, root.Session.ID, "", 100)
	if err != nil || len(page.Reports) != 1 || !reflect.DeepEqual(page.Reports[0], second.Report) {
		t.Fatalf("current judgments: %+v %v", page, err)
	}
	f.drain(t, 128)
	for _, table := range []string{"harness_skill_reads", "df_skill_signals", "df_skill_proposals", "df_skill_generation_inputs"} {
		var count int
		if err := f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("report fabricated %s: %d %v", table, count, err)
		}
	}
	if f.head(t) != beforeHead || f.calls.Load() != 0 {
		t.Fatal("report changed a skill or invoked generation/validation")
	}
	var status string
	if err := f.db.DB().QueryRow(`SELECT status FROM harness_runs WHERE id=?`, child.RunID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("report changed child outcome: %s %v", status, err)
	}
}

func TestMissingReportsCASReplayRedactionAndRestart(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	r := missingRequest(root, root.RunID, "stable")
	r.Summary = "Missing workflow for token=private-first-token."
	first := mustSubmitMissing(t, f.service, r)
	if !first.Report.SummaryRedacted || strings.Contains(first.Report.Summary, "private-first-token") {
		t.Fatalf("credential retained: %+v", first)
	}
	changed := r
	changed.Summary = "Missing workflow for token=private-second-token."
	if _, err := f.service.SubmitMissingReport(ctx, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("redaction collision hid changed original intent", err)
	}
	changed = r
	changed.RequestKey = "new-stale"
	if _, err := f.service.SubmitMissingReport(ctx, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("new request bypassed CAS", err)
	}
	changed.ExpectedReportVersion, changed.RequestKey, changed.Summary = 1, "second", "A revised missing capability."
	mustSubmitMissing(t, f.service, changed)
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again := mustSubmitMissing(t, reopened, r)
	if !again.Replayed || again.CurrentRevision != 2 || !reflect.DeepEqual(again.Report, first.Report) {
		t.Fatalf("restart or later edit broke exact replay: %+v", again)
	}
	var payload []byte
	if err := f.db.DB().QueryRow(`SELECT payload FROM df_skill_missing_revisions WHERE root_id=? AND revision=1`, root.RootBudgetID).Scan(&payload); err != nil || strings.Contains(string(payload), "private-first-token") {
		t.Fatal("persisted raw secret", err)
	}
	// Ownership checks precede replay, even if the request key already exists.
	if _, err := f.db.DB().Exec(`UPDATE budget_members SET session_id='missing-session' WHERE id=?`, root.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.SubmitMissingReport(ctx, r); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("replay bypassed live root/run binding", err)
	}
}

func TestMissingReportsConcurrentCASHasOneWinner(t *testing.T) {
	f := newCollectorFixture(t)
	root := f.run(t, "", 0, "completed", false)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := f.service.SubmitMissingReport(context.Background(), missingRequest(root, root.RunID, key))
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, harness.ErrSkillConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestMissingReportsRejectInvalidRequestsAndForeignRuns(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	other := f.run(t, "", 0, "cancelled", false)
	cases := []struct {
		name string
		edit func(*harness.SkillMissingReportRequest)
		want error
	}{
		{"missing_session", func(r *harness.SkillMissingReportRequest) { r.SessionID = "missing" }, harness.ErrNotFound},
		{"missing_run", func(r *harness.SkillMissingReportRequest) { r.RunID = "missing-role-invocation" }, harness.ErrNotFound},
		{"foreign_run", func(r *harness.SkillMissingReportRequest) { r.RunID = other.RunID }, harness.ErrPermissionDenied},
		{"empty_session", func(r *harness.SkillMissingReportRequest) { r.SessionID = "" }, harness.ErrInvalidInput},
		{"session_untrimmed", func(r *harness.SkillMissingReportRequest) { r.SessionID += " " }, harness.ErrInvalidInput},
		{"run_long", func(r *harness.SkillMissingReportRequest) { r.RunID = strings.Repeat("中", 86) }, harness.ErrInvalidInput},
		{"run_control", func(r *harness.SkillMissingReportRequest) { r.RunID += "\x00" }, harness.ErrInvalidInput},
		{"run_format_control", func(r *harness.SkillMissingReportRequest) { r.RunID += "\u200b" }, harness.ErrInvalidInput},
		{"run_utf8", func(r *harness.SkillMissingReportRequest) { r.RunID = string([]byte{0xff}) }, harness.ErrInvalidInput},
		{"version_negative", func(r *harness.SkillMissingReportRequest) { r.ExpectedReportVersion = -1 }, harness.ErrInvalidInput},
		{"kind_unknown", func(r *harness.SkillMissingReportRequest) { r.Kind = "automatic_failure" }, harness.ErrInvalidInput},
		{"key_empty", func(r *harness.SkillMissingReportRequest) { r.RequestKey = "" }, harness.ErrInvalidInput},
		{"key_space", func(r *harness.SkillMissingReportRequest) { r.RequestKey = "bad key" }, harness.ErrInvalidInput},
		{"summary_empty", func(r *harness.SkillMissingReportRequest) { r.Summary = " \t" }, harness.ErrInvalidInput},
		{"summary_long", func(r *harness.SkillMissingReportRequest) { r.Summary = strings.Repeat("中", 667) }, harness.ErrInvalidInput},
		{"summary_control", func(r *harness.SkillMissingReportRequest) { r.Summary += "\u200b" }, harness.ErrInvalidInput},
		{"withdraw_without_report", func(r *harness.SkillMissingReportRequest) { r.Kind, r.Summary = "withdrawn", "" }, harness.ErrInvalidInput},
		{"withdraw_with_summary", func(r *harness.SkillMissingReportRequest) { r.Kind = "withdrawn" }, harness.ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := missingRequest(root, root.RunID, tc.name)
			tc.edit(&r)
			if _, err := f.service.SubmitMissingReport(ctx, r); !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
		})
	}
	current, err := f.service.MissingReport(ctx, root.Session.ID, root.RunID)
	if err != nil || current.Report != nil {
		t.Fatalf("invalid requests left judgment: %+v %v", current, err)
	}
}

func TestMissingReportsValidateUnconstrainedBudgetBindings(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"source_session", `UPDATE budget_members SET session_id='fake' WHERE id=?`},
		{"root_session", `UPDATE budget_roots SET session_id='fake' WHERE root_run_id=?`},
		{"root_run", `UPDATE budget_roots SET root_run_id='fake' WHERE root_run_id=?`},
		{"root_origin", `UPDATE budget_members SET origin_id='fake' WHERE id=?`},
		{"root_parent", `UPDATE budget_members SET parent_id='fake' WHERE id=?`},
		{"root_kind", `UPDATE budget_members SET kind='task' WHERE id=?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCollectorFixture(t)
			root := f.run(t, "", 0, "completed", false)
			child := f.run(t, root.RootBudgetID, 0, "completed", false)
			id := root.RunID
			if tc.name == "source_session" {
				id = child.RunID
			}
			if _, err := f.db.DB().Exec(tc.sql, id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.SubmitMissingReport(context.Background(), missingRequest(root, child.RunID, "bad-binding")); err == nil {
				t.Fatal("inconsistent runtime/budget graph accepted")
			}
		})
	}
	// Legacy partial background records must still make a child private, even
	// when only one of the two optional background stores is present.
	for _, table := range []string{"harness_background_specs", "harness_background_bindings"} {
		t.Run(table, func(t *testing.T) {
			f := newCollectorFixture(t)
			root := f.run(t, "", 0, "completed", false)
			if _, err := f.db.DB().Exec(`CREATE TABLE `+table+`(child_session_id TEXT); INSERT INTO `+table+` VALUES(?)`, root.Session.ID); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := f.service.SubmitMissingReport(ctx, missingRequest(root, root.RunID, "private")); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("private session accepted as foreground", err)
			}
			if _, err := f.service.QueryMissingReports(ctx, root.Session.ID, "", 10); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("private session listed as foreground", err)
			}
		})
	}
}

func TestMissingReportsPageAndHistoryAreBoundedSnapshots(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	second := missingSecondRoot(t, f, root.Session)
	other := f.run(t, "", 0, "completed", false)
	mustSubmitMissing(t, f.service, missingRequest(root, root.RunID, "one"))
	mustSubmitMissing(t, f.service, missingRequest(second, second.RunID, "two"))
	page, err := f.service.QueryMissingReports(ctx, root.Session.ID, "", 1)
	if err != nil || len(page.Reports) != 1 || page.Next == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	next, err := f.service.QueryMissingReports(ctx, root.Session.ID, page.Next, 1)
	if err != nil || len(next.Reports) != 1 || next.Next != "" || next.Reports[0].RootID == page.Reports[0].RootID {
		t.Fatalf("next page: %+v %v", next, err)
	}
	if _, err := f.service.QueryMissingReports(ctx, other.Session.ID, page.Next, 1); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("cross-session cursor accepted", err)
	}
	r := missingRequest(root, root.RunID, "edit")
	r.ExpectedReportVersion = 1
	mustSubmitMissing(t, f.service, r)
	if _, err := f.service.QueryMissingReports(ctx, root.Session.ID, page.Next, 1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale catalog cursor accepted", err)
	}
	history, err := f.service.QueryMissingReportHistory(ctx, root.Session.ID, root.RunID, "", 1)
	if err != nil || len(history.Revisions) != 1 || history.Next == "" || history.CurrentRevision != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	if _, err := f.service.QueryMissingReportHistory(ctx, root.Session.ID, second.RunID, history.Next, 1); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("cross-root history cursor accepted", err)
	}
	for _, key := range []string{"0", "-1", "01", "1000", "9223372036854775807"} {
		cursor := encodeCursor(pageCursor{Kind: "skill-missing-history-v1", Filter: encodedHash([]string{root.Session.ID, root.RootBudgetID}), Key: key, Revision: 2})
		if _, err := f.service.QueryMissingReportHistory(ctx, root.Session.ID, root.RunID, cursor, 1); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("history cursor %q accepted: %v", key, err)
		}
	}
	r.ExpectedReportVersion, r.RequestKey, r.Kind, r.Summary = 2, "withdraw", "withdrawn", ""
	withdrawn := mustSubmitMissing(t, f.service, r)
	if withdrawn.Report.Kind != "withdrawn" || withdrawn.CurrentRevision != 3 {
		t.Fatalf("withdrawal: %+v", withdrawn)
	}
	if _, err := f.service.QueryMissingReportHistory(ctx, root.Session.ID, root.RunID, history.Next, 1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale history cursor accepted", err)
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := f.service.QueryMissingReports(ctx, root.Session.ID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("list accepted invalid bound", limit, err)
		}
		if _, err := f.service.QueryMissingReportHistory(ctx, root.Session.ID, root.RunID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("history accepted invalid bound", limit, err)
		}
	}
	for _, cursor := range []string{"malformed", strings.Repeat("a", 2049)} {
		if _, err := f.service.QueryMissingReports(ctx, root.Session.ID, cursor, 1); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("bad cursor accepted", err)
		}
	}
	canonical := pageCursor{Kind: "skill-missing-list-v1", Filter: encodedHash([]string{root.Session.ID}), Key: root.RootBudgetID, Revision: 0}
	data, _ := json.Marshal(canonical)
	unknown := base64.RawURLEncoding.EncodeToString(append(data[:len(data)-1], []byte(`,"extra":true}`)...))
	canonical.Kind = "skill-missing-history-v1"
	for _, cursor := range []string{unknown, page.Next[:8] + "\n" + page.Next[8:], encodeCursor(canonical)} {
		if _, err := f.service.QueryMissingReports(ctx, root.Session.ID, cursor, 1); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("noncanonical or cross-purpose cursor accepted", err)
		}
	}
}

func TestMissingReportsStorageFaultRollsBackAndDeletionCascades(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	other := f.run(t, "", 0, "completed", false)
	r := missingRequest(root, root.RunID, "retry")
	if _, err := f.db.DB().Exec(`CREATE TRIGGER missing_fault BEFORE INSERT ON df_skill_missing_revisions BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.SubmitMissingReport(ctx, r); err == nil {
		t.Fatal("injected failure was ignored")
	}
	for _, table := range []string{"df_skill_missing_reports", "df_skill_missing_revisions"} {
		var count int
		if err := f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s survived rollback: %d %v", table, count, err)
		}
	}
	if _, err := f.db.DB().Exec(`DROP TRIGGER missing_fault`); err != nil {
		t.Fatal(err)
	}
	first := mustSubmitMissing(t, f.service, r)
	if first.Replayed || first.CurrentRevision != 1 {
		t.Fatalf("rollback consumed request key: %+v", first)
	}
	retained := mustSubmitMissing(t, f.service, missingRequest(other, other.RunID, "unrelated"))
	runtime := hr.NewService(f.runtime, nil, "fixture")
	if already, err := runtime.DeleteSession(ctx, root.Session.ID); err != nil || already {
		t.Fatalf("production delete: %v %v", already, err)
	}
	if _, err := f.service.SubmitMissingReport(ctx, r); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("delete permitted report replay", err)
	}
	if _, err := f.service.MissingReport(ctx, root.Session.ID, root.RunID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("delete retained report", err)
	}
	for _, table := range []string{"df_skill_missing_reports", "df_skill_missing_revisions"} {
		var count int
		if err := f.db.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("cascade %s count=%d %v", table, count, err)
		}
	}
	current, err := f.service.MissingReport(ctx, other.Session.ID, other.RunID)
	if err != nil || current.Report == nil || !reflect.DeepEqual(*current.Report, retained.Report) {
		t.Fatalf("delete touched unrelated report: %+v %v", current, err)
	}
}

func TestMissingReportsRejectTamperedPayloadOrReboundSource(t *testing.T) {
	for _, mode := range []string{"digest", "attribution", "run_rebound"} {
		t.Run(mode, func(t *testing.T) {
			f := newCollectorFixture(t)
			root := f.run(t, "", 0, "completed", false)
			child := f.run(t, root.RootBudgetID, 0, "completed", false)
			other := f.run(t, "", 0, "completed", false)
			result := mustSubmitMissing(t, f.service, missingRequest(root, child.RunID, "one"))
			switch mode {
			case "digest":
				if _, err := f.db.DB().Exec(`UPDATE df_skill_missing_revisions SET payload='{}'`); err != nil {
					t.Fatal(err)
				}
			case "attribution":
				result.Report.Attribution = "missing_skill_caused_failure"
				data, _ := json.Marshal(result.Report)
				if _, err := f.db.DB().Exec(`UPDATE df_skill_missing_revisions SET payload=?,payload_sha=?`, data, hash(data)); err != nil {
					t.Fatal(err)
				}
			case "run_rebound":
				if _, err := f.db.DB().Exec(`UPDATE budget_members SET root_id=? WHERE id=?`, other.RootBudgetID, child.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.MissingReport(context.Background(), root.Session.ID, root.RunID); err == nil {
				t.Fatal("tampered report was exposed")
			}
		})
	}
}

func TestMissingReportsRevisionCapReservesWithdrawal(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	firstRequest := missingRequest(root, root.RunID, "first")
	first := mustSubmitMissing(t, f.service, firstRequest)
	for revision := int64(1); revision < maxMissingReportRevisions-1; revision++ {
		r := missingRequest(root, root.RunID, fmt.Sprintf("edit-%d", revision))
		r.ExpectedReportVersion = revision
		mustSubmitMissing(t, f.service, r)
	}
	r := missingRequest(root, root.RunID, "over-limit")
	r.ExpectedReportVersion = maxMissingReportRevisions - 1
	if _, err := f.service.SubmitMissingReport(ctx, r); !errors.Is(err, harness.ErrSkillMissingReportLimit) {
		t.Fatal("ordinary report consumed withdrawal reserve", err)
	}
	r.RequestKey, r.Kind, r.Summary = "withdraw", "withdrawn", ""
	mustSubmitMissing(t, f.service, r)
	r.RequestKey, r.ExpectedReportVersion = "after-limit", maxMissingReportRevisions
	if _, err := f.service.SubmitMissingReport(ctx, r); !errors.Is(err, harness.ErrSkillMissingReportLimit) {
		t.Fatal("history exceeded cap", err)
	}
	again := mustSubmitMissing(t, f.service, firstRequest)
	if !again.Replayed || again.CurrentRevision != maxMissingReportRevisions || !reflect.DeepEqual(again.Report, first.Report) {
		t.Fatalf("cap blocked old exact replay: %+v", again)
	}
}
