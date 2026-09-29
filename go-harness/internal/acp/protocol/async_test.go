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
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedEventWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(data), nil
}

func (w *blockedEventWriter) Close() error {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
	return nil
}

func TestAsyncEventQueueIsBoundedAndClosesSlowClient(t *testing.T) {
	in, client := io.Pipe()
	w := &blockedEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	p := NewPeer(in, w, nil, Options{WriteQueueCapacity: 1, MaxQueuedEventBytes: 1 << 20})
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
