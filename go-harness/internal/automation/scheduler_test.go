package automation

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func fixture(t *testing.T, run Runner) *Scheduler {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(context.Background(), db.DB(), run)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestScheduleCalendarTimezonesAndValidation(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	next, err := (Spec{Kind: "cron", Cron: "0 9 * * *", Timezone: "Asia/Shanghai"}).Next(now)
	if err != nil || !next.Equal(now.Add(time.Hour)) {
		t.Fatal(next, err)
	}
	next, err = (Spec{Kind: "cron", Cron: "30 2 * * *", Timezone: "America/New_York"}).Next(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC))
	if err != nil || next.Day() != 9 {
		t.Fatalf("DST gap: %v %v", next, err)
	}
	for _, spec := range []Spec{{Kind: "interval", EverySeconds: 1}, {Kind: "cron", Cron: "@every 1s"}, {Kind: "cron", Cron: "TZ=UTC * * * * *"}, {Kind: "once"}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "invalid"}} {
		if _, err := spec.Next(now); err == nil {
			t.Fatal("invalid spec accepted", spec)
		}
	}
}
func TestScheduleDurableClaimCoalescingNotificationsAndPause(t *testing.T) {
	calls := 0
	s := fixture(t, func(_ context.Context, _ Spec, bind func(string) error) (Outcome, error) {
		calls++
		if err := bind("session-one"); err != nil {
			return Outcome{}, err
		}
		return Outcome{Status: "completed", SessionID: "session-one", Text: "result"}, nil
	})
	ctx := context.Background()
	schedule, err := s.Create(ctx, Spec{Name: "daily", Workspace: t.TempDir(), Prompt: "Read report", Kind: "interval", EverySeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(10 * time.Hour)
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("missed occurrences replayed", calls)
	}
	history, err := s.History(ctx, schedule.Spec.ID)
	if err != nil || len(history) != 1 || history[0].Outcome.Status != "completed" || history[0].Read {
		t.Fatal(history, err)
	}
	if err = s.Acknowledge(ctx, history[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SetPaused(ctx, schedule.Spec.ID, true, currentRevision(t, s, schedule.Spec.ID)); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("paused schedule ran")
	}
	if err = s.SetPaused(ctx, schedule.Spec.ID, false, currentRevision(t, s, schedule.Spec.ID)); err != nil {
		t.Fatal(err)
	}
}
func TestScheduleRestartDoesNotReplayUncertainEffects(t *testing.T) {
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		t.Fatal("uncertain occurrence replayed")
		return Outcome{}, nil
	})
	ctx := context.Background()
	v, err := s.Create(ctx, Spec{Name: "once", Workspace: t.TempDir(), Prompt: "Task", Kind: "once", At: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO harness_schedule_occurrences(id,schedule_id,due_ms,status) VALUES('crashed',?,1,'running')`, v.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, s.db, s.run)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h, err := reopened.History(ctx, v.Spec.ID)
	if err != nil || h[0].Outcome.Status != "unknown" {
		t.Fatal(h, err)
	}
}
func TestScheduleFailurePausesRecurrence(t *testing.T) {
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		return Outcome{SessionID: "failed"}, errors.New("needs permission")
	})
	ctx := context.Background()
	v, err := s.Create(ctx, Spec{Name: "task", Workspace: t.TempDir(), Prompt: "Task", Kind: "interval", EverySeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || !list[0].Paused {
		t.Fatal(list, err)
	}
	h, err := s.History(ctx, v.Spec.ID)
	if err != nil || h[0].Outcome.Status != "needs_attention" {
		t.Fatal(h, err)
	}
}

func TestScheduleDeleteKeepsHistoryAndNotificationFailureStopsReplay(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		calls++
		return Outcome{Status: "completed"}, nil
	})
	v, err := s.Create(ctx, Spec{Name: "once", Workspace: t.TempDir(), Prompt: "task", Kind: "once", At: time.Now().Add(time.Minute), NotifyWeChat: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s.NotifyTx = func(context.Context, *sql.Tx, Spec, Occurrence) error { return errors.New("outbox unavailable") }
	if err = s.Tick(ctx, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("lost notification failure")
	}
	s, err = New(ctx, s.db, s.run)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("effect replayed")
	}
	if err = s.Delete(ctx, v.Spec.ID, currentRevision(t, s, v.Spec.ID)); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(ctx, v.Spec.ID)
	if err != nil || len(h) != 1 || h[0].Outcome.Status != "unknown" {
		t.Fatal(h, err)
	}
}

func TestScheduleOnceSubmillisecondDueDoesNotRunTwice(t *testing.T) {
	calls := 0
	s := fixture(t, func(context.Context, Spec, func(string) error) (Outcome, error) {
		calls++
		return Outcome{Status: "completed"}, nil
	})
	ctx := context.Background()
	at := time.Now().Add(time.Minute).Truncate(time.Millisecond).Add(500 * time.Microsecond)
	_, err := s.Create(ctx, Spec{Name: "precise", Workspace: t.TempDir(), Prompt: "task", Kind: "once", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, at.Truncate(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = s.Tick(ctx, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("once ran twice", calls)
	}
}

func TestScheduleCloseCancelsAndJoinsActiveDispatch(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	s := fixture(t, func(ctx context.Context, _ Spec, bind func(string) error) (Outcome, error) {
		if err := bind("cancelled-session"); err != nil {
			return Outcome{}, err
		}
		close(started)
		<-ctx.Done()
		close(stopped)
		return Outcome{SessionID: "cancelled-session"}, ctx.Err()
	})
	ctx := context.Background()
	v, err := s.Create(ctx, Spec{Name: "shutdown", Workspace: t.TempDir(), Prompt: "Task", Kind: "interval", EverySeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE harness_schedules SET next_ms=1 WHERE id=?`, v.Spec.ID); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	t.Cleanup(s.Close)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler close did not join dispatch")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("close returned before dispatch stopped")
	}
	history, err := s.History(ctx, v.Spec.ID)
	if err != nil || len(history) != 1 || history[0].Outcome.Status != "needs_attention" || history[0].Outcome.SessionID != "cancelled-session" {
		t.Fatal("shutdown lost terminal persistence", history, err)
	}
}
