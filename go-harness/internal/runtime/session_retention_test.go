package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestArchiveProtectsUnfinishedDurableRuns(t *testing.T) {
	for _, status := range []string{"pending", "running", "waiting_input", "suspending", "resuming", "needs_reconciliation", "future_state"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			store, err := NewStore(ctx, native.DB())
			if err != nil {
				t.Fatal(err)
			}
			x, err := store.CreateSession(ctx, t.TempDir(), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().AddDate(0, 0, -6).Format(time.RFC3339Nano)
			if _, err = native.DB().Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, x.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = native.DB().Exec(`INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('run',?,'input',?,?,?)`, x.ID, status, old, old); err != nil {
				t.Fatal(err)
			}
			policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 5, InactiveDays: 5, CheckInterval: time.Hour}
			if archived, _, err := store.archiveSession(ctx, x.ID, time.Now().UTC(), policy); archived || !errors.Is(err, harness.ErrBusy) {
				t.Fatalf("unfinished run archived: %v %v", archived, err)
			}
		})
	}
}

func TestArchiveTransactionFailureAndReopen(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := store.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -6).Format(time.RFC3339Nano)
	if _, err = native.DB().Exec(`UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = native.DB().Exec(`CREATE TRIGGER archive_fault BEFORE UPDATE OF archived_at ON harness_sessions BEGIN SELECT RAISE(ABORT,'archive fault'); END`); err != nil {
		t.Fatal(err)
	}
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 5, InactiveDays: 5, CheckInterval: time.Hour}
	if archived, _, err := store.archiveSession(ctx, x.ID, time.Now().UTC(), policy); err == nil || archived {
		t.Fatalf("archive fault ignored: %v %v", archived, err)
	}
	if expired, err := store.IsExpiredSession(ctx, x.ID, time.Now().UTC(), policy); err != nil || !expired {
		t.Fatalf("failed archive lost retry candidate: %v %v", expired, err)
	}
	if _, err = native.DB().Exec(`DROP TRIGGER archive_fault`); err != nil {
		t.Fatal(err)
	}
	if archived, _, err := store.archiveSession(ctx, x.ID, time.Now().UTC(), policy); err != nil || !archived {
		t.Fatalf("retry archive: %v %v", archived, err)
	}
	if again, _, err := store.archiveSession(ctx, x.ID, time.Now().UTC(), policy); err != nil || again {
		t.Fatalf("repeated archive: %v %v", again, err)
	}
	if err = store.MarkOpen(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if expired, err := store.IsExpiredSession(ctx, x.ID, time.Now().UTC(), policy); err != nil || expired {
		t.Fatalf("reopened archive immediately expired: %v %v", expired, err)
	}
}

func TestSessionRetentionMigratesExistingStore(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if _, err = native.DB().ExecContext(ctx, `CREATE TABLE harness_sessions (id TEXT PRIMARY KEY,cwd TEXT NOT NULL,title TEXT NOT NULL,mode TEXT NOT NULL,model TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := store.CreateSession(ctx, t.TempDir(), "model")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkClosed(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	var closed string
	if err = native.DB().QueryRowContext(ctx, `SELECT closed_at FROM harness_sessions WHERE id=?`, x.ID).Scan(&closed); err != nil || closed == "" {
		t.Fatalf("closed timestamp: %q %v", closed, err)
	}
	if _, err = NewStore(ctx, native.DB()); err != nil {
		t.Fatalf("migration was not idempotent: %v", err)
	}
	if err = store.MarkOpen(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if err = native.DB().QueryRowContext(ctx, `SELECT closed_at FROM harness_sessions WHERE id=?`, x.ID).Scan(&closed); err != nil || closed != "" {
		t.Fatalf("reopened timestamp: %q %v", closed, err)
	}
}

func TestRetentionPageAdvancesAcrossClosedInactiveAndFresh(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	fresh := now.Format(time.RFC3339Nano)
	for _, row := range []struct{ id, updated, closed string }{{"a", fresh, fresh}, {"b", old, ""}, {"c", fresh, ""}} {
		if _, err = native.DB().ExecContext(ctx, `INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at,closed_at) VALUES(?,'/workspace','','default','model',?,?,?)`, row.id, row.updated, row.updated, row.closed); err != nil {
			t.Fatal(err)
		}
	}
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 0, InactiveDays: 30, CheckInterval: time.Hour}
	var collected []string
	cursor := ""
	for {
		ids, next, pageErr := store.ExpiredSessionIDsPage(ctx, now.Add(time.Second), policy, cursor, 1)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		collected = append(collected, ids...)
		if next == "" {
			break
		}
		cursor = next
	}
	if !reflect.DeepEqual(collected, []string{"a", "b"}) {
		t.Fatalf("expired keyset=%v", collected)
	}
	if expired, err := store.IsExpiredSession(ctx, "c", now.Add(time.Second), policy); err != nil || expired {
		t.Fatalf("fresh session expired: %v %v", expired, err)
	}
}

func TestRetentionPolicyRejectsInvalidBounds(t *testing.T) {
	base := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
	for _, policy := range []harness.RetentionPolicy{
		{Enabled: true, ClosedDays: -1, InactiveDays: 30, CheckInterval: time.Hour},
		{Enabled: true, ClosedDays: 30, InactiveDays: 0, CheckInterval: time.Hour},
		{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Second},
		{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: 25 * time.Hour},
	} {
		if err := ValidateRetention(policy); err == nil {
			t.Fatalf("invalid policy accepted: %+v", policy)
		}
	}
	if err := ValidateRetention(base); err != nil {
		t.Fatal(err)
	}
}

func TestRunFinishRefreshesInactiveRetentionClock(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := store.CreateSession(ctx, t.TempDir(), "model")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err = native.DB().ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('retention-run',?,'retention-input','running',?,?)`, x.ID, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = native.DB().ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, old, x.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(ctx, "retention-run", "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	policy := harness.RetentionPolicy{Enabled: true, ClosedDays: 30, InactiveDays: 30, CheckInterval: time.Hour}
	if expired, checkErr := store.IsExpiredSession(ctx, x.ID, time.Now().UTC(), policy); checkErr != nil || expired {
		t.Fatalf("completed run expired immediately: %v %v", expired, checkErr)
	}
}
