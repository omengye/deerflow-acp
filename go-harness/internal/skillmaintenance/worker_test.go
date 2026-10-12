package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestSuccessfulHandlerCanStopAnInFlightHeartbeat(t *testing.T) {
	for _, heartbeatTimeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-shutdown", true: "renewal-timeout"}[heartbeatTimeout], func(t *testing.T) {
			testHeartbeatShutdown(t, heartbeatTimeout)
		})
	}
}

func testHeartbeatShutdown(t *testing.T, heartbeatTimeout bool) {
	t.Helper()
	f := newFixture(t, policy())
	f.store.now = time.Now
	f.db.DB().SetMaxOpenConns(1)
	j := enqueue(t, f.store, "heartbeat-stop")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
