package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// Running the compiled test binary as a child exercises the actual daemon main
// path without a nested, resource-intensive Go build in every package test.
func TestDaemonProcessHelper(t *testing.T) {
	if os.Getenv("DEERFLOW_TEST_DAEMON_HELPER") != "1" {
		return
	}
	for i, value := range os.Args {
		if value == "--" {
			if err := run(os.Args[i+1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

type daemonProcess struct {
	cmd            *exec.Cmd
	done           chan struct{}
	err            error
	stderr, stdout bytes.Buffer
}

func launchDaemon(t *testing.T, dataDir, runtimeDir, baseURL string) *daemonProcess {
	t.Helper()
	p := &daemonProcess{done: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestDaemonProcessHelper$", "--", "--data-dir", dataDir, "--runtime-dir", runtimeDir, "--model", "fixture-model", "--base-url", baseURL, "--max-connections", "2")
	p.cmd.Env = append(os.Environ(), "DEERFLOW_TEST_DAEMON_HELPER=1", "DEERFLOW_MODEL_PROVIDER=openai", "DEERFLOW_MODEL_API_KEY=fixture-only-key")
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *daemonProcess) endpoint(t *testing.T, dir string) localhost.Endpoint {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		b, err := os.ReadFile(filepath.Join(dir, localhost.EndpointFilename))
		var ep localhost.Endpoint
		if err == nil && json.Unmarshal(b, &ep) == nil && ep.PID == p.cmd.Process.Pid {
			return ep
		}
		select {
		case <-p.done:
			t.Fatalf("daemon exited before endpoint: %v; %s", p.err, p.stderr.String())
		case <-deadline.C:
			t.Fatal("daemon startup timeout")
		case <-tick.C:
		}
	}
}

func (p *daemonProcess) wait(t *testing.T, wantSuccess bool) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatal("daemon exit timeout")
	}
	if wantSuccess != (p.err == nil) {
		t.Fatalf("daemon exit: %v; %s", p.err, p.stderr.String())
	}
}

func connectCommand(t *testing.T, ep localhost.Endpoint, command string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = fmt.Fprintf(c, "DFACP/1 %s %s\n", ep.Token, command); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return c, r, strings.TrimSpace(line)
}

type rpcClient struct {
	in   io.Writer
	out  *json.Decoder
	next int
}
type rpcFrame struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (c *rpcClient) send(t *testing.T, method string, params any) int {
	t.Helper()
	c.next++
	if err := json.NewEncoder(c.in).Encode(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	return c.next
}
func (c *rpcClient) request(t *testing.T, method string, params any) rpcFrame {
	t.Helper()
	id := c.send(t, method, params)
	for {
		var f rpcFrame
		if err := c.out.Decode(&f); err != nil {
			t.Fatal(err)
		}
		if f.ID == id {
			return f
		}
	}
}
func (c *rpcClient) success(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	f := c.request(t, method, params)
	if len(f.Error) != 0 {
		t.Fatalf("%s: %s", method, f.Error)
	}
	return f.Result
}
func acpClient(t *testing.T, ep localhost.Endpoint) *rpcClient {
	t.Helper()
	conn, r, line := connectCommand(t, ep, "ACP")
	if line != "OK" {
		t.Fatalf("ACP: %s", line)
	}
	c := &rpcClient{in: conn, out: json.NewDecoder(r)}
	c.success(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	return c
}

func TestDaemonProcessCancellationRestartAndOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("process integration")
	}
	modelStarted, modelStopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Working.\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		close(modelStarted)
		<-r.Context().Done()
		close(modelStopped)
	}))
	t.Cleanup(server.Close)
	dataDir, runtimeDir, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	p := launchDaemon(t, dataDir, runtimeDir, server.URL+"/v1")
	ep := p.endpoint(t, runtimeDir)
	a, b := acpClient(t, ep), acpClient(t, ep)
	created := a.success(t, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.ID == "" {
		t.Fatalf("session: %s %v", created, err)
	}
	if f := b.request(t, "session/load", map[string]any{"cwd": workspace, "sessionId": session.ID, "mcpServers": []any{}}); len(f.Error) == 0 {
		t.Fatal("second ACP client stole session ownership")
	}
	// Sharing a database through a different endpoint directory is rejected.
	duplicate := launchDaemon(t, dataDir, t.TempDir(), server.URL+"/v1")
	duplicate.wait(t, false)
	if !strings.Contains(duplicate.stderr.String(), "data directory already has") {
		t.Fatalf("unexpected duplicate failure: %s", duplicate.stderr.String())
	}
	bad := ep
	bad.Token = "bad-token"
	c, _, line := connectCommand(t, bad, "STOP")
	_ = c.Close()
	if line != "UNAUTHORIZED" {
		t.Fatalf("unauthorized STOP: %s", line)
	}
	c, _, line = connectCommand(t, ep, "STATUS")
	_ = c.Close()
	if !strings.HasSuffix(line, "connections=2") {
		t.Fatal(line)
	}
	a.send(t, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "Run until canceled."}}})
	select {
	case <-modelStarted:
	case <-time.After(15 * time.Second):
		t.Fatal("real Eino provider did not start")
	}
	c, _, line = connectCommand(t, ep, "STOP")
	_ = c.Close()
	if line != "OK" {
		t.Fatal(line)
	}
	p.wait(t, true)
	select {
	case <-modelStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("provider request leaked after daemon exit")
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, localhost.EndpointFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint remains: %v", err)
	}
	if strings.Contains(p.stderr.String(), ep.Token) || p.stdout.Len() != 0 {
		t.Fatal("daemon leaked token or wrote stdout")
	}
	// Successful restart proves both runtime/data ownership locks were released.
	q := launchDaemon(t, dataDir, runtimeDir, server.URL+"/v1")
	next := q.endpoint(t, runtimeDir)
	if next.Token == ep.Token {
		t.Fatal("daemon restart reused bearer credential")
	}
	reloaded := acpClient(t, next)
	reloaded.success(t, "session/load", map[string]any{"cwd": workspace, "sessionId": session.ID, "mcpServers": []any{}})
	c, _, line = connectCommand(t, next, "STOP")
	_ = c.Close()
	if line != "OK" {
		t.Fatal(line)
	}
	q.wait(t, true)
}

func TestRejectPythonConfiguration(t *testing.T) {
	if err := run([]string{"--config", "python-config.yaml"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Python config silently accepted: %v", err)
	}
}

// Supply an existing native Bridge executable to exercise its actual endpoint,
// management and stdio proxy code; no Rust installation/build is needed.
func TestExistingRustBridge(t *testing.T) {
	bridge := os.Getenv("DEERFLOW_TEST_BRIDGE")
	if bridge == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_BRIDGE to a native Bridge executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runtimeDir := t.TempDir()
	p := launchDaemon(t, t.TempDir(), runtimeDir, "http://127.0.0.1:1/v1")
	ep := p.endpoint(t, runtimeDir)
	invoke := func(mode, input string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, bridge, "--no-auto-start", "--runtime-dir", runtimeDir, mode)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Bridge %s: %v %s", mode, err, out)
		}
		if bytes.Contains(out, []byte(ep.Token)) {
			t.Fatal("Bridge leaked token")
		}
		return out
	}
	if out := invoke("--status", ""); !bytes.Contains(out, []byte(buildID)) {
		t.Fatalf("Bridge status: %s", out)
	}
	if out := invoke("--manage", `{"operation":"proposal.list"}`); !bytes.Contains(out, []byte("unsupported_operation")) {
		t.Fatalf("Bridge management: %s", out)
	}
	proxy := exec.CommandContext(ctx, bridge, "--no-auto-start", "--runtime-dir", runtimeDir)
	in, err := proxy.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := proxy.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	proxy.Stderr = &diagnostics
	if err = proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = in.Close()
		if proxy.ProcessState == nil {
			_ = proxy.Process.Kill()
			_ = proxy.Wait()
		}
	}()
	acp := &rpcClient{in: in, out: json.NewDecoder(out)}
	acp.success(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	acp.success(t, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	_ = in.Close()
	if err = proxy.Wait(); err != nil {
		t.Fatalf("Bridge proxy: %v %s", err, diagnostics.String())
	}
	invoke("--stop-daemon", "")
	p.wait(t, true)
}
