package protocol

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestDrainNotificationsWakesEveryConcurrentWaiter(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	c := connection(t, func(context.Context, string, json.RawMessage) (any, error) {
		close(entered)
		<-finish
		return nil, nil
	}, Options{})
	c.write(t, `{"jsonrpc":"2.0","method":"session/cancel"}`)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("notification did not start")
	}
	const waiters = 8
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan error, waiters)
	started := make(chan struct{}, waiters)
	for range waiters {
		go func() {
			started <- struct{}{}
			results <- c.peer.DrainNotifications(ctx)
		}()
	}
	for range waiters {
		<-started
	}
	select {
	case err := <-results:
		t.Fatalf("drained an unfinished notification: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(finish) })
	for range waiters {
		if err := <-results; err != nil {
			t.Fatalf("completed notification stranded another waiter: %v", err)
		}
	}
}
