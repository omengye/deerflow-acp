package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestCompletesBeforeReplyIsPublished(t *testing.T) {
	for _, nextID := range []int{1, 2} {
		t.Run(fmt.Sprintf("next_id_%d", nextID), func(t *testing.T) {
			input, client := io.Pipe()
			writer := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
			var releases atomic.Int32
			calls := make(chan struct{}, 2)
			peer := NewPeer(input, writer, func(context.Context, string, json.RawMessage) (any, error) {
				calls <- struct{}{}
				return nil, nil
			}, Options{MaxConcurrentRequests: 1, Admit: func(ctx context.Context, _ string, _ json.RawMessage) (context.Context, func(), error) {
				return ctx, func() { releases.Add(1) }, nil
			}})
			finished := make(chan error, 1)
			go func() { finished <- peer.Serve(context.Background()) }()
			t.Cleanup(func() {
				peer.Close()
				client.Close()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("peer failed to finish cleanup")
				}
			})
			if _, err := io.WriteString(client, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"first\"}\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-writer.started:
			case <-time.After(5 * time.Second):
				t.Fatal("first response did not reach the writer")
			}
			<-calls
			// A writer can expose a complete response before Write returns.
			// Both ID reuse and the next request must already be admissible.
			peer.mu.Lock()
			active := len(peer.inflight)
			peer.mu.Unlock()
			if active != 0 || len(peer.requestSlots) != 0 || releases.Load() != 1 {
				t.Fatalf("response published before request cleanup: active=%d slots=%d releases=%d", active, len(peer.requestSlots), releases.Load())
			}
			if _, err := fmt.Fprintf(client, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"next\"}\n", nextID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-calls:
			case <-time.After(5 * time.Second):
				t.Fatal("next request was rejected while the finished reply was being written")
			}
		})
	}
}

func TestCompletedReplyQueueIsBounded(t *testing.T) {
	input, client := io.Pipe()
	writer := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	peer := NewPeer(input, writer, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, nil
	}, Options{MaxConcurrentRequests: 1, WriteQueueCapacity: 1})
	finished := make(chan error, 1)
	go func() { finished <- peer.Serve(t.Context()) }()
	t.Cleanup(func() {
		peer.Close()
		client.Close()
	})
	for id := 1; id <= 3; id++ {
		// Dispatch serially and join handler completion, not transport delivery.
		// Only Serve's writer runs; the input reader has no requests to dispatch.
		peer.dispatchRequest(envelope{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: "complete"})
		joined := make(chan struct{})
		go func() { peer.handlers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatal("completed handler waited for the blocked transport")
		}
		if id == 1 {
			select {
			case <-writer.started:
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not start")
			}
		}
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrBackpressure) {
			t.Fatalf("full reply queue ended peer with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("full reply queue did not close the slow peer")
	}
}
