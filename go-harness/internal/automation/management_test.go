package automation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func currentSchedule(t *testing.T, s *Scheduler, id string) Schedule {
	t.Helper()
	rows, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Spec.ID == id {
			return row
		}
	}
	t.Fatal("schedule not found", id)
	return Schedule{}
}

func currentRevision(t *testing.T, s *Scheduler, id string) int64 {
	t.Helper()
	return currentSchedule(t, s, id).Revision
}

func TestScheduleReschedulePreservesIdentityHistoryPermissionsAndPause(t *testing.T) {
	ctx := context.Background()
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		return Outcome{Status: "completed"}, nil
	})
	for _, approval := range []string{"", "read_only", "ask"} {
		v, err := s.Create(ctx, Spec{Name: "task", Workspace: t.TempDir(), Prompt: "unchanged", Kind: "interval", EverySeconds: 60, Approval: approval, NotifyWeChat: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		want := approval
		if want == "" {
			want = "allow_always"
		}
		if v.Spec.Approval != want {
			t.Fatal(v)
		}
		if err = s.SetPaused(ctx, v.Spec.ID, true, v.Revision); err != nil {
			t.Fatal(err)
		}
		v = currentSchedule(t, s, v.Spec.ID)
		old := v.Spec
		next, err := s.Reschedule(ctx, v.Spec.ID, v.Revision, Timing{Kind: "cron", Cron: "0 9 * * 1-5", Timezone: "Asia/Shanghai"}, false)
		if err != nil || !next.Paused || next.PauseReason != "manual" || next.Revision != v.Revision+1 || next.NextLocal == "" {
			t.Fatal(next, err)
		}
		old.Kind, old.Cron, old.Timezone, old.EverySeconds = "cron", "0 9 * * 1-5", "Asia/Shanghai", 0
		if !reflect.DeepEqual(old, next.Spec) {
			t.Fatal("reschedule changed non-timing authority", old, next.Spec)
		}
		if _, err = s.Reschedule(ctx, v.Spec.ID, v.Revision, Timing{Kind: "interval", EverySeconds: 600}, false); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if err = s.Delete(ctx, v.Spec.ID, v.Revision); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if err = s.SetPaused(ctx, v.Spec.ID, false, v.Revision); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if err = s.SetPaused(ctx, v.Spec.ID, false, next.Revision); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScheduleRunningGuardsAndPauseRace(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	s := fixture(t, func(_ context.Context, _ Spec, bind func(string) error) (Outcome, error) {
		if err := bind("owned-session"); err != nil {
			return Outcome{}, err
		}
		close(entered)
		<-release
		return Outcome{Status: "completed", SessionID: "owned-session"}, nil
	})
	v, err := s.Create(ctx, Spec{Name: "run", Workspace: t.TempDir(), Prompt: "task", Kind: "interval", EverySeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Tick(ctx, time.Now().Add(time.Hour)) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	var once sync.Once
	defer once.Do(func() { close(release) })
	active := currentSchedule(t, s, v.Spec.ID)
	if !active.Active || active.Last.Outcome.SessionID != "owned-session" {
		t.Fatal(active)
	}
	if err = s.Delete(ctx, v.Spec.ID, v.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, v.Spec.ID, active.Revision); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, err = s.Reschedule(ctx, v.Spec.ID, active.Revision, Timing{Kind: "interval", EverySeconds: 600}, false); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = s.SetPaused(ctx, v.Spec.ID, true, active.Revision); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	finished := currentSchedule(t, s, v.Spec.ID)
	if finished.Active || !finished.Paused || finished.PauseReason != "manual" || finished.Last.Outcome.Status != "completed" {
		t.Fatal(finished)
	}
	if err = s.Delete(ctx, v.Spec.ID, finished.Revision); err != nil {
		t.Fatal(err)
	}
	history, err := s.History(ctx, v.Spec.ID)
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
}

func TestScheduleRescheduleOnceAndRejectedInputsAreAtomic(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		calls++
		return Outcome{Status: "completed"}, nil
	})
	v, err := s.Create(ctx, Spec{Name: "once", Workspace: t.TempDir(), Prompt: "task", Kind: "once", At: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v = currentSchedule(t, s, v.Spec.ID)
	if v.PauseReason != "exhausted" || !v.Paused || !v.Next.IsZero() {
		t.Fatal(v)
	}
	if err = s.SetPaused(ctx, v.Spec.ID, false, v.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatal("ended once resumed without a new time", err)
	}
	for _, timing := range []Timing{{Kind: "once", At: time.Now().Add(-time.Hour)}, {Kind: "cron", Cron: "bad"}, {Kind: "interval", EverySeconds: 1}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "invalid"}} {
		if _, err = s.Reschedule(ctx, v.Spec.ID, v.Revision, timing, false); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
		if got := currentSchedule(t, s, v.Spec.ID); !reflect.DeepEqual(got, v) {
			t.Fatal("invalid update mutated state")
		}
	}
	next, err := s.Reschedule(ctx, v.Spec.ID, v.Revision, Timing{Kind: "once", At: time.Now().Add(time.Hour), Timezone: "Asia/Shanghai"}, true)
	if err != nil || next.Paused || next.PauseReason != "" || next.Last.ID != v.Last.ID {
		t.Fatal(next, err)
	}
	if err = s.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	history, err := s.History(ctx, v.Spec.ID)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
}

func TestScheduleLegacyMigrationNeverUpgradesPermissions(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.DB().Exec(`CREATE TABLE harness_schedules(id TEXT PRIMARY KEY,spec BLOB NOT NULL,paused INTEGER NOT NULL,next_ms INTEGER NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []string{"", "read_only", "ask"} {
		v := Spec{ID: "legacy-" + grant, Name: "old", Workspace: t.TempDir(), Prompt: "task", Kind: "interval", EverySeconds: 60, Approval: grant}
		raw, _ := json.Marshal(v)
		if _, err = db.DB().Exec(`INSERT INTO harness_schedules VALUES(?,?,1,?)`, v.ID, raw, time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		s, err := New(ctx, db.DB(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.List(ctx)
		if err != nil || len(rows) != 3 {
			t.Fatal(rows, err)
		}
		for _, v := range rows {
			if v.Spec.Approval == "allow_always" || v.Revision != 1 || v.PauseReason != "manual" {
				t.Fatal(v)
			}
		}
	}
}

func TestScheduleConcurrentEditsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	s := fixture(t, nil)
	v, err := s.Create(ctx, Spec{Name: "race", Workspace: t.TempDir(), Prompt: "task", Kind: "interval", EverySeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, n := range []int64{120, 180} {
		go func() {
			_, err := s.Reschedule(ctx, v.Spec.ID, v.Revision, Timing{Kind: "interval", EverySeconds: n}, false)
			results <- err
		}()
	}
	a, b := <-results, <-results
	if !(a == nil && errors.Is(b, ErrConflict) || b == nil && errors.Is(a, ErrConflict)) {
		t.Fatal(a, b)
	}
}

func TestScheduleClaimAndRescheduleSerialize(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
			return Outcome{Status: "completed"}, nil
		})
		v, err := s.Create(ctx, Spec{Name: "race", Workspace: t.TempDir(), Prompt: "inspect", Kind: "once", At: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		start, claimed, edited := make(chan struct{}), make(chan error, 1), make(chan error, 1)
		go func() { <-start; claimed <- s.Tick(ctx, time.Now().Add(2*time.Hour)) }()
		go func() {
			<-start
			_, err := s.Reschedule(ctx, v.Spec.ID, v.Revision, Timing{Kind: "once", At: time.Now().Add(3 * time.Hour)}, false)
			edited <- err
		}()
		close(start)
		claimErr, editErr := <-claimed, <-edited
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		history, err := s.History(ctx, v.Spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if editErr == nil && len(history) != 0 {
			t.Fatal("old time fired after edit committed", history)
		}
		if editErr != nil && (!errors.Is(editErr, ErrConflict) || len(history) != 1) {
			t.Fatal(history, editErr)
		}
	}
}

func TestScheduleDeleteAndClaimSerialize(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
			return Outcome{Status: "completed"}, nil
		})
		v, err := s.Create(ctx, Spec{Name: "delete race", Workspace: t.TempDir(), Prompt: "inspect", Kind: "once", At: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		start, claimed, deleted := make(chan struct{}), make(chan error, 1), make(chan error, 1)
		go func() { <-start; claimed <- s.Tick(ctx, time.Now().Add(2*time.Hour)) }()
		go func() { <-start; deleted <- s.Delete(ctx, v.Spec.ID, v.Revision) }()
		close(start)
		claimErr, deleteErr := <-claimed, <-deleted
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		history, err := s.History(ctx, v.Spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if deleteErr == nil && len(history) != 0 {
			t.Fatal("deleted task fired", history)
		}
		if deleteErr != nil && (!errors.Is(deleteErr, ErrConflict) || len(history) != 1) {
			t.Fatal(history, deleteErr)
		}
	}
}

func TestSchedulePreviewHandlesDST(t *testing.T) {
	now := time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC)
	preview, err := (Timing{Kind: "cron", Cron: "30 2 * * *", Timezone: "America/New_York"}).Preview(now)
	if err != nil || preview.NextLocal != "2026-03-09T02:30:00-04:00" {
		t.Fatal(preview, err)
	}
	preview, err = (Timing{Kind: "once", At: time.Date(2026, 12, 1, 1, 0, 0, 0, time.UTC), Timezone: "Asia/Shanghai"}).Preview(now)
	if err != nil || preview.NextLocal != "2026-12-01T09:00:00+08:00" {
		t.Fatal(preview, err)
	}
}
