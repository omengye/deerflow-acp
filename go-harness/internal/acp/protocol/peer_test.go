package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testConnection struct {
	peer     *Peer
	in       *os.File
	out      *bufio.Reader
	finished chan error
}

// Real OS pipes exercise stdio shutdown and writes beyond pipe capacity.
func connection(t *testing.T, handler Handler, opts Options) *testConnection {
	t.Helper()
	agentIn, clientIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	clientOut, agentOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := NewPeer(agentIn, agentOut, handler, opts)
	c := &testConnection{p, clientIn, bufio.NewReader(clientOut), make(chan error, 1)}
	go func() { c.finished <- p.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = p.Close()
		_ = clientIn.Close()
		_ = clientOut.Close()
		select {
		case <-c.finished:
		case <-time.After(5 * time.Second):
			t.Error("peer failed to finish cleanup")
		}
	})
	return c
}

func (c *testConnection) write(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (c *testConnection) read(t *testing.T) envelope {
	t.Helper()
	type outcome struct {
		msg envelope
		err error
	}
	result := make(chan outcome, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		var msg envelope
		if err == nil {
			err = json.Unmarshal(line, &msg)
		}
		result <- outcome{msg, err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading protocol frame")
	}
	return envelope{}
}

func TestBidirectionalPermissionDuringPrompt(t *testing.T) {
	var peer *Peer
	ready := make(chan struct{})
	c := connection(t, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		<-ready
		if method != "session/prompt" {
			return nil, &Error{Code: MethodNotFound, Message: "Method not found"}
		}
		if err := peer.Notify(ctx, "session/update", map[string]any{"sessionId": "s", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "执行前请授权"}}}); err != nil {
			return nil, err
		}
		var permission struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if err := peer.Call(ctx, "session/request_permission", map[string]any{"sessionId": "s", "options": []any{map[string]any{"optionId": "once", "kind": "allow_once", "name": "Allow"}}}, &permission); err != nil {
			return nil, err
		}
		if permission.Outcome.OptionID != "once" {
			return nil, errors.New("wrong permission")
		}
		return map[string]string{"stopReason": "end_turn"}, nil
	}, Options{MaxConcurrentRequests: 1})
	peer = c.peer
	close(ready)
	// Fragment both UTF-8 content and the JSON frame across writes.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":"你好"}}` + "\n")
	for _, b := range frame {
		if _, err := c.in.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	update := c.read(t)
	if update.Method != "session/update" {
		t.Fatalf("first frame=%+v", update)
	}
	permission := c.read(t)
	if permission.Method != "session/request_permission" {
		t.Fatalf("permission=%+v", permission)
	}
	c.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"outcome":{"outcome":"selected","optionId":"once"}}}`, permission.ID))
	result := c.read(t)
	if string(result.ID) != "1" || string(result.Result) != `{"stopReason":"end_turn"}` {
		t.Fatalf("prompt result=%+v", result)
	}
}

func TestAdmissionRejectsDuplicatePromptWithoutPreemptionAndOrdersCancel(t *testing.T) {
	var mu sync.Mutex
	var active context.CancelFunc
	var activeContext context.Context
	var releases atomic.Int32
	admitted := make(chan struct{}, 1)
	cleanup := make(chan struct{})
	opts := Options{Admit: func(ctx context.Context, method string, _ json.RawMessage) (context.Context, func(), error) {
		if method != "session/prompt" {
			return ctx, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if active != nil {
			return nil, nil, &Error{Code: ServerBusy, Message: "Session busy"}
		}
		activeContext, active = context.WithCancel(ctx)
		admitted <- struct{}{}
		return activeContext, func() { mu.Lock(); active = nil; mu.Unlock(); releases.Add(1) }, nil
	}}
	c := connection(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "session/prompt":
			<-ctx.Done()
			<-cleanup
			return map[string]string{"stopReason": "cancelled"}, nil
		case "session/cancel":
			if !IsNotification(ctx) {
				return nil, errors.New("cancel is a notification")
			}
			mu.Lock()
			if active != nil {
				active()
			}
			mu.Unlock()
		}
		return nil, nil
	}, opts)
	c.write(t, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s"}}`)
	<-admitted
	c.write(t, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s"}}`)
	busy := c.read(t)
	if string(busy.ID) != "2" || busy.Error == nil || busy.Error.Code != ServerBusy {
		t.Fatalf("duplicate result=%+v", busy)
	}
	mu.Lock()
	err := activeContext.Err()
	mu.Unlock()
	if err != nil {
		t.Fatal("duplicate prompt canceled first prompt")
	}
	c.write(t, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`)
	// Cleanup remains part of the occupied session.
	c.write(t, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s"}}`)
	busy = c.read(t)
	if string(busy.ID) != "3" || busy.Error == nil {
		t.Fatalf("cleanup admission=%+v", busy)
	}
	close(cleanup)
	cancelled := c.read(t)
	if string(cancelled.ID) != "1" || string(cancelled.Result) != `{"stopReason":"cancelled"}` {
		t.Fatalf("cancel result=%+v", cancelled)
	}
	_ = c.peer.Close()
	select {
	case <-c.peer.Done():
	case <-time.After(time.Second):
		t.Fatal("not closed")
	}
}

func TestCancelImmediatelyAfterPromptSeesAdmission(t *testing.T) {
	var active context.CancelFunc
	var mu sync.Mutex
	c := connection(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "session/cancel" {
			mu.Lock()
			active()
			mu.Unlock()
			return nil, nil
		}
		<-ctx.Done()
		return map[string]string{"stopReason": "cancelled"}, nil
	}, Options{Admit: func(ctx context.Context, _ string, _ json.RawMessage) (context.Context, func(), error) {
		mu.Lock()
		var requestCtx context.Context
		requestCtx, active = context.WithCancel(ctx)
		mu.Unlock()
		return requestCtx, nil, nil
	}})
	c.write(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"session/prompt\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"session/cancel\"}")
	got := c.read(t)
	if string(got.Result) != `{"stopReason":"cancelled"}` {
		t.Fatalf("response=%+v", got)
	}
}

func TestEOFUnblocksPermissionAndWaitsForCleanup(t *testing.T) {
	var peer *Peer
	ready := make(chan struct{})
	callFinished := make(chan error, 1)
	cleanup := make(chan struct{})
	c := connection(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		<-ready
		err := peer.Call(ctx, "session/request_permission", map[string]string{"sessionId": "s"}, nil)
		callFinished <- err
		<-cleanup
		return nil, err
	}, Options{})
	peer = c.peer
	close(ready)
	c.write(t, `{"jsonrpc":"2.0","id":1,"method":"session/prompt"}`)
	_ = c.read(t)
	_ = c.in.Close()
	select {
	case err := <-callFinished:
		if err == nil {
			t.Fatal("permission approved on disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("permission not canceled")
	}
	select {
	case err := <-c.finished:
		c.finished <- err
		t.Fatal("Serve returned before handler cleanup")
	default:
	}
	close(cleanup)
	select {
	case err := <-c.finished:
		c.finished <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish")
	}
}

func TestJSONRPCErrorsAndUnknownMethod(t *testing.T) {
	c := connection(t, nil, Options{})
	for _, tt := range []struct {
		line string
		code int
		id   string
	}{
		{`{"jsonrpc":`, ParseError, "null"},
		{`[]`, InvalidRequest, "null"},
		{`{"jsonrpc":"1.0","id":9,"method":"x"}`, InvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":true,"method":"x"}`, InvalidRequest, "null"},
		{`{"jsonrpc":"2.0","id":"string-id","method":"x","params":true}`, InvalidParams, `"string-id"`},
		{`{"jsonrpc":"2.0","id":9007199254740993,"method":"unknown"}`, MethodNotFound, "9007199254740993"},
	} {
		c.write(t, tt.line)
		got := c.read(t)
		if got.Error == nil || got.Error.Code != tt.code || string(got.ID) != tt.id {
			t.Fatalf("line=%s response=%+v", tt.line, got)
		}
	}
}

func TestFrameLimitAndTruncatedFrame(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		want        error
	}{
		{"oversize", strings.Repeat("x", 257) + "\n", ErrFrameTooLarge},
		{"unterminated", `{"jsonrpc":"2.0"}`, ErrTruncatedFrame},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := connection(t, nil, Options{MaxFrameBytes: 256})
			_, _ = io.WriteString(c.in, tt.input)
			_ = c.in.Close()
			select {
			case err := <-c.finished:
				c.finished <- err
				if !errors.Is(err, tt.want) {
					t.Fatalf("err=%v want=%v", err, tt.want)
				}
			case <-time.After(time.Second):
				t.Fatal("no frame failure")
			}
		})
	}
	// Exercise the actual default 64 MiB boundary without retaining JSON copies.
	large := io.MultiReader(io.LimitReader(zeroReader{}, int64(DefaultMaxFrameBytes)), strings.NewReader("\n"))
	frame, err := readFrame(bufio.NewReaderSize(large, 64<<10), DefaultMaxFrameBytes)
	if err != nil || len(frame) != DefaultMaxFrameBytes {
		t.Fatalf("64 MiB boundary: len=%d err=%v", len(frame), err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestInvalidUTF8ProducesParseError(t *testing.T) {
	c := connection(t, nil, Options{})
	frame := append([]byte(`{"jsonrpc":"2.0","method":"`), 0xff)
	frame = append(frame, []byte("\"}\n")...)
	_, _ = c.in.Write(frame)
	if got := c.read(t); got.Error == nil || got.Error.Code != ParseError {
		t.Fatalf("response=%+v", got)
	}
}

func TestConcurrentNotificationsDoNotInterleave(t *testing.T) {
	c := connection(t, nil, Options{})
	const count = 30
	errorsCh := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			errorsCh <- c.peer.Notify(context.Background(), "session/update", map[string]any{"n": i, "text": strings.Repeat("字", 20000)})
		}()
	}
	seen := make(map[int]bool)
	for i := 0; i < count; i++ {
		msg := c.read(t)
		var params struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatal(err)
		}
		seen[params.N] = true
	}
	for i := 0; i < count; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != count {
		t.Fatalf("received %d/%d unique frames", len(seen), count)
	}
}

func TestCanceledCallReleasesPendingSlot(t *testing.T) {
	c := connection(t, nil, Options{MaxPendingCalls: 1})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		first <- c.peer.Call(ctx, "session/request_permission", map[string]string{"sessionId": "s"}, nil)
	}()
	request := c.read(t)
	if err := c.peer.Call(context.Background(), "x", nil, nil); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("limit error=%v", err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	// A late reply for a canceled approval cannot complete a subsequent call.
	c.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"approved":true}}`, request.ID))
	second := make(chan error, 1)
	go func() {
		second <- c.peer.Call(context.Background(), "session/request_permission", map[string]string{"sessionId": "s"}, nil)
	}()
	request2 := c.read(t)
	if bytes.Equal(request.ID, request2.ID) {
		t.Fatal("request ID reused")
	}
	c.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unsupported"}}`, request2.ID))
	var rpc *Error
	if err := <-second; !errors.As(err, &rpc) || rpc.Code != MethodNotFound {
		t.Fatalf("second error=%v", err)
	}
}

func TestCloseUnblocksBackpressuredWriter(t *testing.T) {
	in, clientIn := io.Pipe()
	clientOut, out := io.Pipe()
	defer clientIn.Close()
	defer clientOut.Close()
	p := NewPeer(in, out, nil, Options{WriteQueueCapacity: 1})
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- p.Serve(ctx) }()
	notifyDone := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			notifyDone <- p.Notify(context.Background(), "session/update", map[string]string{"text": "blocked"})
		}()
	}
	cancel()
	select {
	case err := <-serveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer prevented shutdown")
	}
	for i := 0; i < 3; i++ {
		select {
		case err := <-notifyDone:
			if err == nil {
				t.Fatal("unread notification succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("notification leaked")
		}
	}
}

func TestNumericIDsPreservePrecisionAndCanonicalize(t *testing.T) {
	for _, pair := range [][2]string{{"1", "1.0"}, {"12300", "1.2300e4"}, {"-0", "0e123"}, {"10e999999999", "1e1000000000"}} {
		a, err := idKey(json.RawMessage(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := idKey(json.RawMessage(pair[1]))
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("equal numeric IDs differ: %s=%s, %s=%s", pair[0], a, pair[1], b)
		}
	}
	a, _ := idKey(json.RawMessage("9007199254740992"))
	b, _ := idKey(json.RawMessage("9007199254740993"))
	if a == b {
		t.Fatal("large integer IDs lost precision")
	}
	b, _ = idKey(json.RawMessage(`"9007199254740992"`))
	if a == b {
		t.Fatal("string and numeric IDs collided")
	}
}

func TestExactConfiguredFrameLimitAccepted(t *testing.T) {
	c := connection(t, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }, Options{MaxFrameBytes: 256})
	frame := `{"jsonrpc":"2.0","id":1,"method":"x"}`
	c.write(t, frame+strings.Repeat(" ", 256-len(frame)))
	got := c.read(t)
	if got.Error != nil || string(got.Result) != "null" {
		t.Fatalf("result=%+v", got)
	}
}

func TestNotificationOverflowClosesAndCancelsHandler(t *testing.T) {
	started := make(chan struct{})
	c := connection(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}, Options{NotificationQueueSize: 1})
	c.write(t, `{"jsonrpc":"2.0","method":"notification"}`)
	<-started
	c.write(t, "{\"jsonrpc\":\"2.0\",\"method\":\"notification\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notification\"}")
	select {
	case err := <-c.finished:
		c.finished <- err
		if !errors.Is(err, ErrBackpressure) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow did not close connection")
	}
}

func TestOrdinaryErrorsAndPanicsAreSanitized(t *testing.T) {
	c := connection(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "panic" {
			panic("private token")
		}
		return nil, errors.New("private filesystem path")
	}, Options{})
	for _, method := range []string{"error", "panic"} {
		c.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method))
		got := c.read(t)
		if got.Error == nil || got.Error.Code != InternalError || got.Error.Message != "Internal error" || got.Error.Data != nil {
			t.Fatalf("response=%+v", got)
		}
	}
}
