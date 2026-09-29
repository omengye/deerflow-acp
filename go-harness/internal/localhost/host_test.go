package localhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startHost(t *testing.T, cfg Config) (*Host, Endpoint) {
	t.Helper()
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = t.TempDir()
	}
	if cfg.ServeACP == nil {
		cfg.ServeACP = func(_ context.Context, in io.ReadCloser, _ io.WriteCloser) error {
			_, err := io.Copy(io.Discard, in)
			return err
		}
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := h.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("close host: %v", err)
		}
	})
	return h, ep
}

func dial(t *testing.T, ep Endpoint) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c, bufio.NewReader(c)
}

func command(t *testing.T, ep Endpoint, cmd string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, r := dial(t, ep)
	if _, err := fmt.Fprintf(c, "%s %s %s\n", handshakeVersion, ep.Token, cmd); err != nil {
		t.Fatal(err)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return c, r, strings.TrimSpace(line)
}

func await(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: %s", label)
	}
}

func TestAuthenticationAndBoundedHandshake(t *testing.T) {
	_, ep := startHost(t, Config{HandshakeTimeout: 150 * time.Millisecond})
	for _, cmd := range []string{"ACP", "STOP", "STATUS", "MANAGE"} {
		bad := ep
		bad.Token = "unauthorized"
		c, _, reply := command(t, bad, cmd)
		_ = c.Close()
		if reply != "UNAUTHORIZED" {
			t.Fatalf("%s: %q", cmd, reply)
		}
	}
	for _, payload := range []string{"DFACP/2 " + ep.Token + " STOP\n", "DFACP/1 " + ep.Token + " STOP extra\n", strings.Repeat("x", 4097) + "\n"} {
		c, r := dial(t, ep)
		_, _ = io.WriteString(c, payload)
		line, err := r.ReadString('\n')
		_ = c.Close()
		if err != nil || line != "UNAUTHORIZED\n" {
			t.Fatalf("malformed handshake: %q %v", line, err)
		}
	}
	// A client that never finishes its first line cannot linger indefinitely.
	c, r := dial(t, ep)
	_, _ = io.WriteString(c, "DFACP/1 ")
	start := time.Now()
	_, err := r.ReadString('\n')
	_ = c.Close()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("slow handshake was not bounded: %v", err)
	}
	c, _, line := command(t, ep, "STATUS")
	_ = c.Close()
	if !strings.HasPrefix(line, "OK ") {
		t.Fatalf("unauthorized STOP affected host: %s", line)
	}
}

func TestPipelinedHandshakeRetainsACPBytes(t *testing.T) {
	_, ep := startHost(t, Config{ServeACP: func(_ context.Context, in io.ReadCloser, out io.WriteCloser) error {
		_, err := io.CopyN(out, in, int64(len("{\"jsonrpc\":\"2.0\"}\n")))
		return err
	}})
	c, r := dial(t, ep)
	payload := "{\"jsonrpc\":\"2.0\"}\n"
	_, err := fmt.Fprintf(c, "DFACP/1 %s ACP\n%s", ep.Token, payload)
	if err != nil {
		t.Fatal(err)
	}
	line, err := r.ReadString('\n')
	if err != nil || line != "OK\n" {
		t.Fatalf("handshake: %q %v", line, err)
	}
	line, err = r.ReadString('\n')
	if err != nil || line != payload {
		t.Fatalf("pipelined frame lost: %q %v", line, err)
	}
}

func TestControlsBypassIdleACPLimitAndStopJoinsCleanup(t *testing.T) {
	dir := t.TempDir()
	started, canceled, release, handlerDone, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	h, ep := startHost(t, Config{RuntimeDir: dir, MaxConnections: 1,
		ServeACP: func(ctx context.Context, _ io.ReadCloser, _ io.WriteCloser) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			close(handlerDone)
			return ctx.Err()
		},
		Cleanup: func() error {
			select {
			case <-handlerDone:
			default:
				return errors.New("cleanup ran before handler finished")
			}
			if _, err := os.Stat(filepath.Join(dir, EndpointFilename)); err != nil {
				return err
			}
			close(cleaned)
			return nil
		},
	})
	// Ensure even a failed assertion releases this deliberately blocked handler.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	_, _, line := command(t, ep, "ACP")
	if line != "OK" {
		t.Fatalf("ACP: %s", line)
	}
	await(t, started, "handler start")
	c, _, line := command(t, ep, "ACP")
	_ = c.Close()
	if line != "BUSY" {
		t.Fatalf("capacity: %s", line)
	}
	c, _, line = command(t, ep, "STATUS")
	_ = c.Close()
	if !strings.HasSuffix(line, "connections=1") {
		t.Fatalf("status: %s", line)
	}
	// Idle handshake and management sockets also must be closed by STOP.
	slow, _ := dial(t, ep)
	_, _ = io.WriteString(slow, "DFACP/1 ")
	_, _, line = command(t, ep, "MANAGE")
	if line != "OK" {
		t.Fatal(line)
	}
	c, _, line = command(t, ep, "STOP")
	_ = c.Close()
	if line != "OK" {
		t.Fatalf("STOP: %s", line)
	}
	await(t, canceled, "run cancellation")
	select {
	case <-h.done:
		t.Fatal("host finished before callback cleanup")
	default:
	}
	if _, err := os.Stat(filepath.Join(dir, EndpointFilename)); err != nil {
		t.Fatalf("endpoint removed before cleanup: %v", err)
	}
	close(release)
	await(t, h.done, "orderly shutdown")
	await(t, cleaned, "client cleanup")
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, EndpointFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint remains: %v", err)
	}
}

func TestManagementUnsupportedAndInvalidRequests(t *testing.T) {
	_, ep := startHost(t, Config{})
	for _, tc := range []struct{ request, code string }{
		{`{"op":"session.list"}`, "unsupported_operation"},
		{`{"op":"proposal.apply"}`, "unsupported_operation"},
		{`[]`, "invalid_request"}, {`null`, "invalid_request"}, {`{broken`, "invalid_request"},
		{`{"op":"` + strings.Repeat("x", 64<<10) + `"}`, "invalid_request"},
	} {
		c, r := dial(t, ep)
		_, err := fmt.Fprintf(c, "DFACP/1 %s MANAGE\n%s\n", ep.Token, tc.request)
		if err != nil {
			t.Fatal(err)
		}
		line, err := r.ReadString('\n')
		if err != nil || line != "OK\n" {
			t.Fatalf("handshake: %q %v", line, err)
		}
		var response struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r).Decode(&response); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if response.OK || response.Code != tc.code {
			t.Fatalf("response: %+v", response)
		}
	}
}

func TestRuntimeLockStaleEndpointAndOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, EndpointFilename), []byte(`{"pid":-1,"token":"stale"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h, ep := startHost(t, Config{RuntimeDir: dir})
	other, err := New(Config{RuntimeDir: dir, ServeACP: h.cfg.ServeACP})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Start(context.Background()); err == nil {
		_ = other.Close()
		t.Fatal("second host acquired runtime lock")
	}
	b, err := os.ReadFile(filepath.Join(dir, EndpointFilename))
	if err != nil {
		t.Fatal(err)
	}
	var published Endpoint
	if err = json.Unmarshal(b, &published); err != nil || published != ep {
		t.Fatal("failed starter modified endpoint")
	}
	// Simulate a successor identity: shutdown must preserve it even with same PID.
	successor := ep
	successor.Token = "successor"
	if err = publishEndpoint(dir, successor); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(dir, EndpointFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &published); err != nil || published.Token != successor.Token {
		t.Fatal("successor endpoint was removed")
	}
	if _, err = other.Start(context.Background()); err != nil {
		t.Fatalf("runtime lock was not released: %v", err)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentConnectionsAndCleanupErrors(t *testing.T) {
	var calls atomic.Int32
	wantErr := errors.New("cleanup fixture")
	h, err := New(Config{RuntimeDir: t.TempDir(), MaxConnections: 2,
		ServeACP: func(ctx context.Context, _ io.ReadCloser, _ io.WriteCloser) error {
			calls.Add(1)
			<-ctx.Done()
			return nil
		},
		Cleanup: func() error { return wantErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := h.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := 0; i < 2; i++ {
		_, _, reply := command(t, ep, "ACP")
		if reply != "OK" {
			t.Fatalf("connection %d: %s", i, reply)
		}
	}
	c, _, reply := command(t, ep, "STATUS")
	_ = c.Close()
	if !strings.HasSuffix(reply, "connections=2") {
		t.Fatal(reply)
	}
	if err = h.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("cleanup error lost: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("callbacks: %d", calls.Load())
	}
	if err = h.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("idempotent close error: %v", err)
	}
}
