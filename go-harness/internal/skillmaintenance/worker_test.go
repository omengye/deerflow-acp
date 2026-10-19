package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type heartbeatSQLiteError int

func (e heartbeatSQLiteError) Error() string { return fmt.Sprintf("SQLite code %d", e) }
func (e heartbeatSQLiteError) Code() int     { return int(e) }

func TestHeartbeatCancellationClassificationPreservesStorageFailures(t *testing.T) {
	for _, item := range []struct {
		name                   string
		workErr, checkErr, err error
		want                   bool
	}{
		{"context-cancelled", context.Canceled, context.Canceled, context.Canceled, true},
		{"sqlite-interrupt", context.Canceled, context.Canceled, fmt.Errorf("renewal: %w", heartbeatSQLiteError(9)), true},
		{"job-deadline", context.DeadlineExceeded, context.DeadlineExceeded, heartbeatSQLiteError(9), true},
		{"live-interrupt", nil, nil, heartbeatSQLiteError(9), false},
		{"renewal-timeout", nil, context.DeadlineExceeded, heartbeatSQLiteError(9), false},
		{"renewal-expired-before-stop", context.Canceled, context.DeadlineExceeded, heartbeatSQLiteError(9), false},
		{"storage-failure-after-stop", context.Canceled, context.Canceled, heartbeatSQLiteError(10), false},
		{"unrelated-error", context.Canceled, context.Canceled, errors.New("disk failure"), false},
	} {
		t.Run(item.name, func(t *testing.T) {
			if got := heartbeatStoppedByWork(item.workErr, item.checkErr, item.err); got != item.want {
				t.Fatalf("stopped=%v, want %v", got, item.want)
			}
		})
	}
}

func TestSuccessfulHandlerCanStopAnInFlightHeartbeat(t *testing.T) {
	for _, heartbeatTimeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-shutdown", true: "renewal-timeout"}[heartbeatTimeout], func(t *testing.T) {
			testHeartbeatShutdown(t, heartbeatTimeout)
		})
	}
}

func TestHeartbeatWaitsForBusyConnectionWithinLease(t *testing.T) {
	f := newFixture(t, policy())
	f.store.now = time.Now
	j := enqueue(t, f.store, "busy-connection")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := f.db.DB().Stats().WaitCount
	worked, err := f.store.RunOne(ctx, func(work context.Context, _ Job) (string, bool) {
		conn, err := f.db.DB().Conn(work)
		if err != nil {
			t.Error(err)
			return "storage_error", false
		}
		defer conn.Close()
		var previous int64
		if err := conn.QueryRowContext(work, `SELECT lease_ns FROM df_skill_maintenance_jobs WHERE id=?`, j.ID).Scan(&previous); err != nil {
			t.Error(err)
			return "storage_error", false
		}
		for f.db.DB().Stats().WaitCount == before && work.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		// A publication transaction may occupy the only SQLite connection for
		// more than two seconds while the worker still owns a valid lease.
		select {
		case <-time.After(3 * time.Second):
		case <-work.Done():
			t.Error("connection contention cancelled a live lease", work.Err())
			return "storage_error", false
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
			return "storage_error", false
		}
		for work.Err() == nil {
			var renewed int64
			if err := f.db.DB().QueryRowContext(work, `SELECT lease_ns FROM df_skill_maintenance_jobs WHERE id=?`, j.ID).Scan(&renewed); err != nil {
				t.Error(err)
				return "storage_error", false
			}
			if renewed > previous {
				return "", false
			}
			time.Sleep(time.Millisecond)
		}
		return "storage_error", false
	})
	if err != nil || !worked {
		t.Fatalf("live lease did not recover after connection release: worked=%v err=%v", worked, err)
	}
	state, err := f.store.Get(ctx, j.ID)
	if err != nil || state.State != "completed" || state.ErrorCode != "" || state.Attempts != 1 {
		t.Fatalf("contention retried successful work: %+v %v", state, err)
	}
}

func TestHeartbeatStorageFailureCancelsWork(t *testing.T) {
	f := newFixture(t, policy())
	f.store.now = time.Now
	j := enqueue(t, f.store, "heartbeat-fault")
	if _, err := f.db.DB().Exec(`CREATE TRIGGER heartbeat_fault BEFORE UPDATE OF lease_ns ON df_skill_maintenance_jobs
WHEN OLD.state='running' AND NEW.lease_ns>OLD.lease_ns BEGIN SELECT RAISE(ABORT,'heartbeat storage fault'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	worked, err := f.store.RunOne(ctx, func(work context.Context, _ Job) (string, bool) {
		<-work.Done()
		return "", false
	})
	if !worked || err == nil || !strings.Contains(err.Error(), "heartbeat storage fault") || ctx.Err() != nil {
		t.Fatalf("storage failure did not stop work: worked=%v err=%v parent=%v", worked, err, ctx.Err())
	}
	state, err := f.store.Get(ctx, j.ID)
	if err != nil || state.State != "retry" || state.ErrorCode != "storage_error" {
		t.Fatalf("storage failure was hidden: %+v %v", state, err)
	}
}

func TestHeartbeatFinalLeaseKeepsJobDeadlineClassification(t *testing.T) {
	p := policy()
	p.MaxJobSeconds = 7
	f := newFixture(t, p)
	f.store.now = time.Now
	j := enqueue(t, f.store, "final-lease")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	worked, err := f.store.RunOne(ctx, func(work context.Context, _ Job) (string, bool) {
		conn, err := f.db.DB().Conn(work)
		if err != nil {
			t.Error(err)
			return "storage_error", false
		}
		defer conn.Close()
		<-work.Done()
		return "", false
	})
	if !worked || err != nil {
		t.Fatalf("job deadline became a storage error: worked=%v err=%v", worked, err)
	}
	state, err := f.store.Get(ctx, j.ID)
	if err != nil || state.State != "failed" || state.ErrorCode != "job_deadline" || state.Attempts != 1 {
		t.Fatalf("job deadline retried: %+v %v", state, err)
	}
}

func testHeartbeatShutdown(t *testing.T, heartbeatTimeout bool) {
	t.Helper()
	f := newFixture(t, policy())
	f.store.now = time.Now
	f.db.DB().SetMaxOpenConns(1)
	j := enqueue(t, f.store, "heartbeat-stop")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	held := make(chan *sql.Conn, 1)
	done := make(chan error, 1)
	before := f.db.DB().Stats().WaitCount
	go func() {
		_, err := f.store.RunOne(ctx, func(work context.Context, _ Job) (string, bool) {
			conn, err := f.db.DB().Conn(work)
			if err != nil {
				t.Error(err)
				return "storage_error", false
			}
			held <- conn
			// Complete while the heartbeat is waiting for our connection. Keep
			// it held until RunOne has cancelled and joined that heartbeat.
			for f.db.DB().Stats().WaitCount == before && work.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			if heartbeatTimeout {
				<-work.Done()
			}
			return "", false
		})
		done <- err
	}()
	var conn *sql.Conn
	select {
	case conn = <-held:
		defer conn.Close()
	case <-ctx.Done():
		t.Fatal("handler did not start")
	}
	// The second waiter is final settlement, after the heartbeat has joined.
	for f.db.DB().Stats().WaitCount < before+2 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("worker did not join its stopped heartbeat")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if heartbeatTimeout {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("real heartbeat timeout was swallowed: %v", err)
		}
	} else if err != nil {
		t.Fatalf("successful handler reported its own cleanup cancellation: %v", err)
	}
	state, err := f.store.Get(ctx, j.ID)
	wantState, wantCode := "completed", ""
	if heartbeatTimeout {
		wantState, wantCode = "retry", "storage_error"
	}
	if err != nil || state.State != wantState || state.ErrorCode != wantCode {
		t.Fatalf("incorrect heartbeat outcome: %+v %v", state, err)
	}
}
