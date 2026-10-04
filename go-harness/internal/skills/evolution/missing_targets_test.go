package evolution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func missingTargetRun(t *testing.T, f *collectorFixture, session harness.Session, id, root, kind, created string) harness.RunRequest {
	t.Helper()
	ctx := context.Background()
	request := harness.RunRequest{Session: session, RunID: id, InputID: hr.NewID(), RootBudgetID: root,
		Input: []harness.Content{{Type: "text", Text: "PRIVATE_TASK_PROMPT"}}}
	if root == "" {
		request.RootBudgetID = id
	}
	if err := f.runtime.BeginRun(ctx, request); err != nil {
		t.Fatal(err)
	}
	var err error
	if root == "" {
		err = f.ledger.CreateRoot(ctx, budget.RootSpec{RootBudgetID: id, RootRunID: id, SessionID: session.ID})
	} else {
		err = f.ledger.BindMember(ctx, budget.MemberBinding{RootBudgetID: root, MemberID: id, SessionID: session.ID, OriginRunID: root, ParentMemberID: root, Kind: kind})
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.DB().Exec(`UPDATE harness_runs SET created_at=?,updated_at=?,error='PRIVATE_PROVIDER_ERROR' WHERE id=?`, created, created, id); err != nil {
		t.Fatal(err)
	}
	return request
}

func missingTargetSession(t *testing.T, f *collectorFixture) harness.Session {
	t.Helper()
	x, err := f.runtime.CreateSession(context.Background(), f.cfg.Sources[0].Workspace, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func targetIDs(page harness.SkillMissingReportTargetPage) []string {
	ids := make([]string, 0, len(page.Targets))
	for _, target := range page.Targets {
		ids = append(ids, target.RootID)
	}
	return ids
}

func TestMissingReportTargetsDiscoverUnreadTasksAndDeduplicateDescendants(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	root := f.run(t, "", 0, "completed", false)
	child := f.run(t, root.RunID, 0, "failed", false)
	missingTargetRun(t, f, root.Session, "continuation", root.RunID, "continuation", "2026-10-02T11:00:00Z")
	missingTargetRun(t, f, child.Session, "background-continuation", root.RunID, "background_continuation", "2026-10-02T12:00:00Z")
	other := f.run(t, "", 0, "completed", false)
	page, err := f.service.QueryMissingReportTargets(ctx, root.Session.ID, "", 20)
	if err != nil || len(page.Targets) != 1 || page.Next != "" {
		t.Fatalf("targets=%+v err=%v", page, err)
	}
	target := page.Targets[0]
	if target.RootID != root.RunID || target.RootRunID != root.RunID || target.RootSessionID != root.Session.ID || target.Status != "completed" || target.ReportRevision != 0 {
		t.Fatalf("target=%+v", target)
	}
	for _, table := range []string{"harness_skill_reads", "df_skill_missing_reports", "df_skill_missing_revisions", "df_skill_signals"} {
		var count int
		if err := f.db.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("discovery wrote %s: count=%d err=%v", table, count, err)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("discovery invoked review model")
	}
	otherPage, err := f.service.QueryMissingReportTargets(ctx, other.Session.ID, "", 20)
	if err != nil || len(otherPage.Targets) != 1 || otherPage.Targets[0].RootRunID != other.RunID {
		t.Fatalf("other session=%+v err=%v", otherPage, err)
	}
	_, err = f.service.SubmitMissingReport(ctx, harness.SkillMissingReportRequest{SessionID: root.Session.ID, RunID: child.RunID,
		RequestKey: "missing-target-report", Kind: "missing_skill", Summary: "A reusable capability is missing."})
	if err != nil {
		t.Fatal(err)
	}
	page, err = f.service.QueryMissingReportTargets(ctx, root.Session.ID, "", 20)
	if err != nil || len(page.Targets) != 1 || page.Targets[0].ReportRevision != 1 || page.Targets[0].RootRunID != root.RunID {
		t.Fatalf("reported target=%+v err=%v", page, err)
	}
	data, err := json.Marshal(page)
	if err != nil || strings.Contains(string(data), "PRIVATE_") || strings.Contains(string(data), "reusable capability") {
		t.Fatalf("target metadata leaked task/report content: %s err=%v", data, err)
	}
}

func TestMissingReportTargetsChronologicalLiveKeyset(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	session := missingTargetSession(t, f)
	for _, entry := range []struct{ id, created string }{
		{"root-a", "2026-10-02T10:00:00.1Z"},
		{"root-c", "2026-10-02T10:00:00.12Z"},
		{"root-b", "2026-10-02T10:00:00.12Z"},
		{"root-d", "2026-10-02T10:00:00Z"},
	} {
		missingTargetRun(t, f, session, entry.id, "", "", entry.created)
	}
	first, err := f.service.QueryMissingReportTargets(ctx, session.ID, "", 2)
	if err != nil || !reflect.DeepEqual(targetIDs(first), []string{"root-c", "root-b"}) || first.Next == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// A newly accepted root is visible on refresh. Neither a later update to
	// an old root nor a new report invalidates the live keyset continuation.
	missingTargetRun(t, f, session, "root-new", "", "", "2026-10-02T10:00:00.123456789Z")
	if _, err := f.db.DB().Exec(`UPDATE harness_runs SET status='waiting_input',updated_at='2099-01-01T00:00:00Z' WHERE id='root-d'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SubmitMissingReport(ctx, harness.SkillMissingReportRequest{SessionID: session.ID, RunID: "root-a",
		RequestKey: "target-live-revision", Kind: "missing_skill", Summary: "Need a reusable workflow."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.QueryMissingReportTargets(ctx, session.ID, first.Next, 2)
	if err != nil || !reflect.DeepEqual(targetIDs(second), []string{"root-a", "root-d"}) || second.Next != "" || second.Targets[0].ReportRevision != 1 || second.Targets[1].Status != "waiting_input" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	refreshed, err := f.service.QueryMissingReportTargets(ctx, session.ID, "", 100)
	if err != nil || !reflect.DeepEqual(targetIDs(refreshed), []string{"root-new", "root-c", "root-b", "root-a", "root-d"}) {
		t.Fatalf("refreshed=%+v err=%v", refreshed, err)
	}
}

func TestMissingReportTargetsRejectInvalidQueriesAndCursors(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	session := missingTargetSession(t, f)
	missingTargetRun(t, f, session, "root-a", "", "", "2026-10-02T10:00:00Z")
	missingTargetRun(t, f, session, "root-b", "", "", "2026-10-02T11:00:00Z")
	first, err := f.service.QueryMissingReportTargets(ctx, session.ID, "", 1)
	if err != nil || first.Next == "" {
		t.Fatal(first, err)
	}
	data, err := base64.RawURLEncoding.DecodeString(first.Next)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := decodeMissingTargetCursor(first.Next, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrongKind := cursor
	wrongKind.Kind = "skill-missing-list-v1"
	badTime := cursor
	badTime.CreatedAt = "2026-10-02T11:00:00+00:00"
	badRoot := cursor
	badRoot.RootID = " root-b"
	unknown := append(append([]byte{}, data[:len(data)-1]...), []byte(`,"extra":true}`)...)
	duplicate := append(append([]byte{}, data[:len(data)-1]...), []byte(`,"root_id":"root-b"}`)...)
	for _, after := range []string{
		"not-base64", strings.Repeat("a", 2049), base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		first.Next + "\n", first.Next + "=", base64.RawURLEncoding.EncodeToString(unknown),
		base64.RawURLEncoding.EncodeToString(duplicate), encodeMissingTargetCursor(wrongKind),
		encodeMissingTargetCursor(badTime), encodeMissingTargetCursor(badRoot),
	} {
		if _, err := f.service.QueryMissingReportTargets(ctx, session.ID, after, 1); err == nil {
			t.Fatalf("accepted malformed cursor %q", after)
		}
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := f.service.QueryMissingReportTargets(ctx, session.ID, "", limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	for _, id := range []string{"", " " + session.ID, "invalid\x00session"} {
		if _, err := f.service.QueryMissingReportTargets(ctx, id, "", 1); err == nil {
			t.Fatalf("accepted session %q", id)
		}
	}
	other := missingTargetSession(t, f)
	if _, err := f.service.QueryMissingReportTargets(ctx, other.ID, first.Next, 1); err == nil {
		t.Fatal("accepted another session's cursor")
	}
	empty, err := f.service.QueryMissingReportTargets(ctx, other.ID, "", 1)
	if err != nil || empty.Targets == nil || len(empty.Targets) != 0 || empty.Next != "" {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	if _, err := f.service.QueryMissingReportTargets(ctx, "missing-session", "", 1); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("missing session error=%v", err)
	}
}

func TestMissingReportTargetsRejectPrivateSessions(t *testing.T) {
	for _, table := range []string{"harness_background_specs", "harness_background_bindings"} {
		t.Run(table, func(t *testing.T) {
			f := newCollectorFixture(t)
			session := missingTargetSession(t, f)
			missingTargetRun(t, f, session, "private-root", "", "", "2026-10-02T10:00:00Z")
			// Isolate each optional privacy index. Native task creation and its
			// complete binding payload are covered by runtime/SDK integration.
			if _, err := f.db.DB().Exec("CREATE TABLE " + table + "(child_session_id TEXT NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB().Exec("INSERT INTO "+table+" VALUES(?)", session.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.QueryMissingReportTargets(context.Background(), session.ID, "", 1); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("private session error=%v", err)
			}
		})
	}
}

func TestMissingReportTargetsDeletedOrChangedAnchor(t *testing.T) {
	for _, mutation := range []string{"root-deleted", "creation-changed", "session-deleted"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCollectorFixture(t)
			ctx := context.Background()
			session := missingTargetSession(t, f)
			missingTargetRun(t, f, session, "root-a", "", "", "2026-10-02T10:00:00Z")
			missingTargetRun(t, f, session, "root-b", "", "", "2026-10-02T11:00:00Z")
			first, err := f.service.QueryMissingReportTargets(ctx, session.ID, "", 1)
			if err != nil || first.Next == "" {
				t.Fatal(first, err)
			}
			want := harness.ErrSkillConflict
			switch mutation {
			case "root-deleted":
				for _, query := range []string{`DELETE FROM budget_members WHERE root_id='root-b'`, `DELETE FROM budget_roots WHERE id='root-b'`} {
					if _, err := f.db.DB().Exec(query); err != nil {
						t.Fatal(err)
					}
				}
			case "creation-changed":
				if _, err := f.db.DB().Exec(`UPDATE harness_runs SET created_at='2026-10-02T12:00:00Z' WHERE id='root-b'`); err != nil {
					t.Fatal(err)
				}
			case "session-deleted":
				want = harness.ErrNotFound
				for _, query := range []string{
					`DELETE FROM budget_members WHERE session_id=?`, `DELETE FROM budget_roots WHERE session_id=?`,
					`DELETE FROM harness_events WHERE session_id=?`,
					`DELETE FROM harness_inputs WHERE run_id IN (SELECT id FROM harness_runs WHERE session_id=?)`,
					`DELETE FROM harness_runs WHERE session_id=?`, `DELETE FROM harness_sessions WHERE id=?`,
				} {
					if _, err := f.db.DB().Exec(query, session.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.service.QueryMissingReportTargets(ctx, session.ID, first.Next, 1); !errors.Is(err, want) {
				t.Fatalf("anchor error=%v want=%v", err, want)
			}
		})
	}
}

func TestMissingReportTargetsRejectDamagedRootMetadata(t *testing.T) {
	for _, query := range []string{
		`UPDATE budget_members SET origin_id='different' WHERE id='root'`,
		`UPDATE budget_members SET session_id='different' WHERE id='root'`,
		`UPDATE harness_runs SET created_at='2026-10-02T10:00:00+00:00' WHERE id='root'`,
		`UPDATE harness_runs SET status='PRIVATE_PROVIDER_STATE' WHERE id='root'`,
	} {
		t.Run(query, func(t *testing.T) {
			f := newCollectorFixture(t)
			session := missingTargetSession(t, f)
			missingTargetRun(t, f, session, "root", "", "", "2026-10-02T10:00:00Z")
			if _, err := f.db.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.QueryMissingReportTargets(context.Background(), session.ID, "", 1); err == nil || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("damaged target error=%v", err)
			}
		})
	}
}
