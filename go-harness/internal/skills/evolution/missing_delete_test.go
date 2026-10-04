package evolution

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func TestMissingReportConcurrentSessionDeletionLeavesNoOrphans(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	other := f.run(t, "", 0, "completed", false)
	unrelated := mustSubmitMissing(t, f.service, missingRequest(other, other.RunID, "retained"))
	runtime := hr.NewService(f.runtime, nil, "fixture")
	request := missingRequest(root, root.RunID, "competing-submit")

	type submitOutcome struct {
		result harness.SkillMissingReportResult
		err    error
	}
	type deleteOutcome struct {
		already bool
		err     error
	}
	submitted := make(chan submitOutcome, 1)
	deleted := make(chan deleteOutcome, 1)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	go func() {
		ready.Done()
		<-start
		result, err := f.service.SubmitMissingReport(ctx, request)
		submitted <- submitOutcome{result, err}
	}()
	go func() {
		ready.Done()
		<-start
		already, err := runtime.DeleteSession(ctx, root.Session.ID)
		deleted <- deleteOutcome{already, err}
	}()
	ready.Wait()
	close(start)
	// SQLite serializes the two transactions. Either the report commits first
	// and is cascaded away, or deletion wins and the report finds no real task.
	submit, deletion := <-submitted, <-deleted
	if deletion.err != nil || deletion.already {
		t.Fatalf("concurrent deletion: %+v", deletion)
	}
	if submit.err != nil && !errors.Is(submit.err, harness.ErrNotFound) {
		t.Fatalf("unexpected concurrent report outcome: %+v", submit)
	}
	if submit.err == nil && (submit.result.CurrentRevision != 1 || submit.result.Replayed) {
		t.Fatalf("winning report write was inconsistent: %+v", submit.result)
	}
	for _, query := range []struct{ sql, id string }{
		{`SELECT count(*) FROM harness_sessions WHERE id=?`, root.Session.ID},
		{`SELECT count(*) FROM harness_runs WHERE session_id=?`, root.Session.ID},
		{`SELECT count(*) FROM budget_roots WHERE session_id=?`, root.Session.ID},
		{`SELECT count(*) FROM budget_members WHERE root_id=?`, root.RootBudgetID},
		{`SELECT count(*) FROM df_skill_missing_reports WHERE root_id=?`, root.RootBudgetID},
		{`SELECT count(*) FROM df_skill_missing_revisions WHERE root_id=?`, root.RootBudgetID},
	} {
		var count int
		if err := f.db.DB().QueryRow(query.sql, query.id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("concurrent delete retained rows: query=%s count=%d err=%v", query.sql, count, err)
		}
	}
	var orphans int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_missing_revisions r
 LEFT JOIN df_skill_missing_reports h ON h.root_id=r.root_id
 LEFT JOIN budget_roots b ON b.id=h.root_id
 LEFT JOIN harness_sessions s ON s.id=h.session_id
 WHERE h.root_id IS NULL OR b.id IS NULL OR s.id IS NULL`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("orphan report revisions: %d %v", orphans, err)
	}
	if _, err := f.service.SubmitMissingReport(ctx, request); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("request replay recreated a deleted session report", err)
	}
	current, err := f.service.MissingReport(ctx, other.Session.ID, other.RunID)
	if err != nil || current.Report == nil || !reflect.DeepEqual(*current.Report, unrelated.Report) {
		t.Fatalf("concurrent deletion changed unrelated report: %+v %v", current, err)
	}
	history, err := f.service.QueryMissingReportHistory(ctx, other.Session.ID, other.RunID, "", 100)
	if err != nil || history.CurrentRevision != 1 || len(history.Revisions) != 1 || !reflect.DeepEqual(history.Revisions[0], unrelated.Report) {
		t.Fatalf("concurrent deletion changed unrelated history: %+v %v", history, err)
	}
}
