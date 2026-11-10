package channels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

type independentPollHandler struct{}

func (independentPollHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return channelproto.Descriptor{ID: "fixture", Name: "Fixture", Version: "1", Protocol: 1, IndependentPoll: true}, nil
	case "poll":
		time.Sleep(30 * time.Second)
		return channelproto.Batch{}, nil
	case "send":
		var in channelproto.Send
		if err := channelproto.Decode(raw, &in); err != nil || in.Text != "answer" || in.Key == "" {
			return nil, errors.New("invalid fixture answer")
		}
		return map[string]bool{"sent": true}, nil
	default:
		return nil, errors.New("unexpected fixture method")
	}
}

func TestIndependentPollPluginChild(t *testing.T) {
	if os.Args[len(os.Args)-1] != "independent-poll-fixture" {
		return
	}
	if err := channelproto.Serve(context.Background(), os.Stdin, os.Stdout, independentPollHandler{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type observedPollTransport struct {
	transport
	once    sync.Once
	entered chan struct{}
}

func (p *observedPollTransport) Call(ctx context.Context, method string, in, out any) error {
	if method == "poll" {
		p.once.Do(func() { close(p.entered) })
	}
	return p.transport.Call(ctx, method, in, out)
}

func TestRealIndependentPollProcessDoesNotBlockAnswer(t *testing.T) {
	entered := make(chan struct{})
	m, i := fixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		select {
		case <-entered:
			return automation.Outcome{Status: "completed", Text: "answer"}, nil
		case <-ctx.Done():
			return automation.Outcome{}, ctx.Err()
		}
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	i.manifest = Manifest{ID: "fixture", Command: exe, Args: []string{"-test.run=^TestIndependentPollPluginChild$", "independent-poll-fixture"}, SHA256: hex.EncodeToString(sum[:])}
	var processes atomic.Int32
	m.start = func(ctx context.Context, manifest Manifest) (transport, channelproto.Descriptor, error) {
		p, descriptor, err := startProcess(ctx, manifest)
		if err != nil {
			return nil, descriptor, err
		}
		processes.Add(1)
		return &observedPollTransport{transport: p, entered: entered}, descriptor, nil
	}
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.startWorker(i)
	waitMessageState(t, m, "a", "sent")
	if processes.Load() != 2 {
		t.Fatal("expected independent serial send and receive processes", processes.Load())
	}
	closed := make(chan struct{})
	go func() { m.stopWorker(i); close(closed) }()
	waitChannel(t, closed, "reaping long-poll process on shutdown")
}
