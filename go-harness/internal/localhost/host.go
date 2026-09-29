package localhost

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const handshakeVersion = "DFACP/1"

type Config struct {
	RuntimeDir string
	BuildID    string
	ConfigPath string
	// MaxConnections limits accepted ACP streams, not concurrent model runs.
	// Control commands never consume these slots, even when every ACP is idle.
	MaxConnections    int
	HandshakeTimeout  time.Duration
	ManagementTimeout time.Duration
	ServeACP          func(context.Context, io.ReadCloser, io.WriteCloser) error
	Manage            func(context.Context, ManagementRequest) (any, error)
	// Cleanup runs after every connection callback has returned, while this
	// process still owns endpoint.json and daemon.lock. Typically Client.Close.
	Cleanup func() error
}

type ManagementRequest struct {
	Operation         string
	Fields            map[string]json.RawMessage
	ActiveConnections int
}

type ManagementError struct{ Code, Message string }

func (e *ManagementError) Error() string { return e.Message }

type Host struct {
	cfg         Config
	mu          sync.Mutex
	started     bool
	closed      bool
	ctx         context.Context
	cancel      context.CancelFunc
	listener    net.Listener
	lock        *flock.Flock
	dir         string
	endpoint    Endpoint
	connections map[net.Conn]struct{}
	active      int
	wg          sync.WaitGroup
	done        chan struct{}
	err         error
}

func New(cfg Config) (*Host, error) {
	if cfg.ServeACP == nil {
		return nil, fmt.Errorf("ServeACP is required")
	}
	if cfg.MaxConnections < 0 || cfg.HandshakeTimeout < 0 || cfg.ManagementTimeout < 0 {
		return nil, fmt.Errorf("limits and timeouts must not be negative")
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 32
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 3 * time.Second
	}
	if cfg.ManagementTimeout == 0 {
		cfg.ManagementTimeout = 10 * time.Second
	}
	if cfg.BuildID == "" {
		cfg.BuildID = "deerflow-go-dev"
	}
	if words := strings.Fields(cfg.BuildID); len(words) != 1 || words[0] != cfg.BuildID {
		return nil, fmt.Errorf("build ID must be one nonempty word")
	}
	return &Host{cfg: cfg, connections: make(map[net.Conn]struct{}), done: make(chan struct{})}, nil
}

// Start publishes the endpoint only after binding a loopback listener and
// acquiring the exclusive runtime lock. A Host may only be started once.
func (h *Host) Start(ctx context.Context) (Endpoint, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started || h.closed {
		return Endpoint{}, fmt.Errorf("daemon host already started or closed")
	}
	if err := ctx.Err(); err != nil {
		return Endpoint{}, err
	}
	dir, err := prepareRuntimeDir(h.cfg.RuntimeDir)
	if err != nil {
		return Endpoint{}, err
	}
	lock := flock.New(filepath.Join(dir, "daemon.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return Endpoint{}, err
	}
	if !locked {
		_ = lock.Close()
		return Endpoint{}, fmt.Errorf("runtime directory already has a daemon: %s", dir)
	}
	if err = securePath(filepath.Join(dir, "daemon.lock"), false); err != nil {
		_ = lock.Close()
		return Endpoint{}, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = lock.Close()
		return Endpoint{}, err
	}
	token, err := newToken()
	if err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return Endpoint{}, err
	}
	ep := Endpoint{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Token: token, PID: os.Getpid(), BuildID: h.cfg.BuildID, ConfigPath: h.cfg.ConfigPath}
	if err = publishEndpoint(dir, ep); err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return Endpoint{}, err
	}
	h.dir, h.lock, h.listener, h.endpoint = dir, lock, listener, ep
	h.ctx, h.cancel = context.WithCancel(ctx)
	h.started = true
	go h.serve()
	return ep, nil
}

// Wait returns only after ACP callbacks, Cleanup, endpoint removal and lock
// release. A handler must honor cancellation and complete its own cleanup.
func (h *Host) Wait() error {
	h.mu.Lock()
	started, closed := h.started, h.closed
	h.mu.Unlock()
	if !started && !closed {
		return fmt.Errorf("daemon host is not started")
	}
	<-h.done
	return h.err
}

func (h *Host) Close() error {
	h.mu.Lock()
	if !h.started && !h.closed {
		h.closed = true
		close(h.done)
	}
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return h.Wait()
}

func (h *Host) stopConnections() {
	_ = h.listener.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.connections {
		_ = conn.Close()
	}
}

func (h *Host) serve() {
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); <-h.ctx.Done(); h.stopConnections() }()
	var serveErr error
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if h.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				serveErr = err
			}
			break
		}
		h.mu.Lock()
		if h.ctx.Err() != nil {
			h.mu.Unlock()
			_ = conn.Close()
			break
		}
		h.connections[conn] = struct{}{}
		h.wg.Add(1)
		h.mu.Unlock()
		go func() {
			defer h.wg.Done()
			defer func() { _ = conn.Close(); h.mu.Lock(); delete(h.connections, conn); h.mu.Unlock() }()
			h.handle(conn)
		}()
	}
	h.cancel()
	<-watchDone
	h.wg.Wait()
	var cleanupErr error
	if h.cfg.Cleanup != nil {
		cleanupErr = h.cfg.Cleanup()
	}
	h.err = errors.Join(serveErr, cleanupErr, removeOwnedEndpoint(h.dir, h.endpoint), h.lock.Close())
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	close(h.done)
}

func (h *Host) handle(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	_ = conn.SetDeadline(time.Now().Add(h.cfg.HandshakeTimeout))
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(reader, 4096)
	if err != nil {
		_, _ = io.WriteString(conn, "UNAUTHORIZED\n")
		return
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != handshakeVersion || subtle.ConstantTimeCompare([]byte(parts[1]), []byte(h.endpoint.Token)) != 1 {
		_, _ = io.WriteString(conn, "UNAUTHORIZED\n")
		return
	}
	switch strings.ToUpper(parts[2]) {
	case "STATUS":
		h.mu.Lock()
		active := h.active
		h.mu.Unlock()
		_, _ = fmt.Fprintf(conn, "OK %d %s connections=%d\n", h.endpoint.PID, h.endpoint.BuildID, active)
	case "STOP":
		// Acknowledge before cancellation closes this control socket.
		_, _ = io.WriteString(conn, "OK\n")
		h.cancel()
	case "MANAGE":
		if _, err = io.WriteString(conn, "OK\n"); err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(h.cfg.ManagementTimeout))
		h.manage(reader, conn)
	case "ACP":
		h.mu.Lock()
		if h.ctx.Err() != nil {
			h.mu.Unlock()
			_, _ = io.WriteString(conn, "STOPPING\n")
			return
		}
		if h.active >= h.cfg.MaxConnections {
			h.mu.Unlock()
			_, _ = io.WriteString(conn, "BUSY\n")
			return
		}
		h.active++
		h.mu.Unlock()
		defer func() { h.mu.Lock(); h.active--; h.mu.Unlock() }()
		if _, err = io.WriteString(conn, "OK\n"); err != nil {
			return
		}
		_ = conn.SetDeadline(time.Time{})
		// The reader may already contain an ACP frame sent in the handshake
		// packet. Passing conn directly would silently discard those bytes.
		_ = h.cfg.ServeACP(h.ctx, bufferedConnection{Reader: reader, Closer: conn}, conn)
	default:
		_, _ = io.WriteString(conn, "ERROR unsupported-command\n")
	}
}

type bufferedConnection struct {
	io.Reader
	io.Closer
}

func readLine(reader *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return "", fmt.Errorf("line exceeds limit")
		}
		line = append(line, part...)
		if err == nil {
			return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

func (h *Host) manage(reader *bufio.Reader, out io.Writer) {
	line, err := readLine(reader, 64<<10)
	var request map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal([]byte(line), &request)
	}
	if err != nil || request == nil {
		_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": "expected one JSON object line of at most 64 KiB", "code": "invalid_request"})
		return
	}
	var operation string
	if raw, ok := request["operation"]; ok {
		if json.Unmarshal(raw, &operation) != nil || len(operation) > 128 {
			_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": "invalid management operation", "code": "invalid_request"})
			return
		}
	}
	if operation == "" || h.cfg.Manage == nil {
		_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": "unsupported management operation", "code": "unsupported_operation"})
		return
	}
	h.mu.Lock()
	active := h.active
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, h.cfg.ManagementTimeout)
	defer cancel()
	data, err := h.cfg.Manage(ctx, ManagementRequest{Operation: operation, Fields: request, ActiveConnections: active})
	if err != nil {
		var known *ManagementError
		code, message := "operation_failed", "management operation failed"
		if errors.As(err, &known) {
			code, message = known.Code, known.Message
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": message, "code": code})
		return
	}
	_ = json.NewEncoder(out).Encode(map[string]any{"ok": true, "data": data})
}
