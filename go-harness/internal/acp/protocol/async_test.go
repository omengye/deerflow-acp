package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestAsyncEventSurvivesPromptCancellation(t *testing.T) {
	in, clientIn := io.Pipe()
	clientOut, out := io.Pipe()
	p := NewPeer(in, out, nil, Options{})
	finished := make(chan error, 1)
	go func() { finished <- p.Serve(context.Background()) }()
	defer clientIn.Close()
	defer clientOut.Close()
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.NotifyAsync(ctx, "session/update", map[string]string{"text": "durable"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	read := make(chan []byte, 1)
	go func() {
		line, _ := bufio.NewReader(clientOut).ReadBytes('\n')
		read <- line
	}()
	select {
	case line := <-read:
		if len(line) == 0 || !json.Valid(line) {
			t.Fatalf("accepted event was lost after prompt cancellation: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted event was not delivered")
	}
	_ = p.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("peer did not close")
	}
}

func TestAsyncEventPrecedesPromptResponse(t *testing.T) {
	var peer *Peer
	c := connection(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method != "session/prompt" {
			return nil, ErrClosed
		}
		if err := peer.NotifyAsync(ctx, "session/update", map[string]string{"text": "first"}); err != nil {
			return nil, err
		}
		return map[string]string{"stopReason": "end_turn"}, nil
	}, Options{})
	peer = c.peer
	c.write(t, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`)
	first := c.read(t)
	if first.Method != "session/update" || first.ID != nil {
		t.Fatalf("event did not precede response: %+v", first)
	}
	second := c.read(t)
	if string(second.ID) != "1" || second.Error != nil {
		t.Fatalf("prompt response: %+v", second)
	}
}

type blockedEventWriter struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (w *blockedEventWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(data), nil
}

func (w *blockedEventWriter) Close() error {
	w.releaseOnce.Do(func() { close(w.release) })
	return nil
}

func TestAsyncEventQueueIsBoundedAndClosesStalledClientAfterTimeout(t *testing.T) {
	in, client := io.Pipe()
	w := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	p := NewPeer(in, w, nil, Options{WriteQueueCapacity: 1, MaxQueuedEventBytes: 1 << 20, EventQueueTimeout: 40 * time.Millisecond})
	finished := make(chan error, 1)
	go func() { finished <- p.Serve(context.Background()) }()
	defer client.Close()
	defer p.Close()
	if err := p.NotifyAsync(context.Background(), "session/update", map[string]string{"text": "one"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not begin")
	}
	if err := p.NotifyAsync(context.Background(), "session/update", map[string]string{"text": "two"}); err != nil {
		t.Fatal(err)
	}
	if err := p.NotifyAsync(context.Background(), "session/update", map[string]string{"text": "three"}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("unbounded async event queue: %v", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrBackpressure) {
			t.Fatalf("peer ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backpressure failed to close peer")
	}
}

func TestAsyncEventByteBudget(t *testing.T) {
	in, client := io.Pipe()
	w := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	p := NewPeer(in, w, nil, Options{MaxQueuedEventBytes: 32})
	finished := make(chan error, 1)
	go func() { finished <- p.Serve(context.Background()) }()
	defer client.Close()
	defer p.Close()
	if err := p.NotifyAsync(context.Background(), "session/update", map[string]string{"text": "this event exceeds a tiny byte budget"}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("byte budget was ignored: %v", err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, ErrBackpressure) {
			t.Fatalf("peer ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("byte budget failed to close peer")
	}
}

func TestAsyncEventBackpressureRecoversWithoutDisconnect(t *testing.T) {
	for _, limit := range []string{"frames", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			in, client := io.Pipe()
			defer client.Close()
			w := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
			options := Options{WriteQueueCapacity: 1, EventQueueTimeout: time.Second}
			if limit == "bytes" {
				frame, err := marshalFrame(envelope{JSONRPC: "2.0", Method: "event"}, DefaultMaxFrameBytes)
				if err != nil {
					t.Fatal(err)
				}
				options.MaxQueuedEventBytes = int64(len(frame))
			}
			p := NewPeer(in, w, nil, options)
			defer p.Close()
			finished := make(chan error, 1)
			go func() { finished <- p.Serve(context.Background()) }()
			if err := p.NotifyAsync(context.Background(), "event", nil); err != nil {
				t.Fatal(err)
			}
			<-w.started
			if limit == "frames" {
				if err := p.NotifyAsync(context.Background(), "event", nil); err != nil {
					t.Fatal(err)
				}
			}
			pending := make(chan error, 1)
			go func() { pending <- p.NotifyAsync(context.Background(), "event", nil) }()
			select {
			case err := <-pending:
				t.Fatalf("full %s budget failed without waiting: %v", limit, err)
			case <-time.After(20 * time.Millisecond):
			}
			w.Close()
			select {
			case err := <-pending:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("event did not resume after client recovered")
			}
			if p.ctx.Err() != nil {
				t.Fatal("slow client disconnected")
			}
			p.Close()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("peer did not close")
			}
		})
	}
}

func TestAsyncBackpressureCancellationReleasesReservationAndPreservesPeer(t *testing.T) {
	in, client := io.Pipe()
	defer client.Close()
	w := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	p := NewPeer(in, w, nil, Options{WriteQueueCapacity: 1, EventQueueTimeout: time.Second})
	defer p.Close()
	finished := make(chan error, 1)
	go func() { finished <- p.Serve(context.Background()) }()
	if err := p.NotifyAsync(context.Background(), "event", nil); err != nil {
		t.Fatal(err)
	}
	<-w.started
	if err := p.NotifyAsync(context.Background(), "event", nil); err != nil {
		t.Fatal(err)
	}
	reserved := p.queuedEvents.Load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := make(chan error, 1)
	go func() { pending <- p.NotifyAsync(ctx, "event", nil) }()
	select {
	case err := <-pending:
		t.Fatalf("full queue failed without waiting: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-pending:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock admission")
	}
	if p.ctx.Err() != nil || p.queuedEvents.Load() != reserved {
		t.Fatal("cancellation closed peer or leaked event capacity")
	}
	go func() { pending <- p.NotifyAsync(context.Background(), "event", nil) }()
	p.Close()
	select {
	case err := <-pending:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock admission")
	}
	<-finished
}

func TestPromptResponseWaitsBehindFullEventQueue(t *testing.T) {
	var peer *Peer
	queued := make(chan struct{})
	c := connection(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		for _, text := range []string{"first", "second"} {
			if err := peer.NotifyAsync(ctx, "session/update", map[string]string{"text": text}); err != nil {
				return nil, err
			}
		}
		close(queued)
		return map[string]string{"stopReason": "end_turn"}, nil
	}, Options{WriteQueueCapacity: 1, EventQueueTimeout: time.Second})
	peer = c.peer
	c.write(t, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`)
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("events did not fill queue")
	}
	select {
	case <-peer.Done():
		t.Fatal("reply overflow disconnected the session")
	case <-time.After(20 * time.Millisecond):
	}
	for i := 0; i < 2; i++ {
		if msg := c.read(t); msg.Method != "session/update" {
			t.Fatalf("out of order event: %+v", msg)
		}
	}
	if msg := c.read(t); string(msg.ID) != "1" || msg.Error != nil {
		t.Fatalf("missing final response: %+v", msg)
	}
}
