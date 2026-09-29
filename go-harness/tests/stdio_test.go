package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type wireProcess struct {
	cmd         *exec.Cmd
	input       io.WriteCloser
	frames      chan frame
	diagnostics *bytes.Buffer
	done        chan error
	next        int
	updates     []json.RawMessage
}

func launch(t *testing.T, binary, dataDir, endpoint string) *wireProcess {
	t.Helper()
	cmd := exec.Command(binary, "--data-dir", dataDir, "--model", "fixture-model", "--base-url", endpoint)
	cmd.Env = append(os.Environ(), "DEERFLOW_MODEL_API_KEY=fixture-only-key", "DEERFLOW_MODEL_PROVIDER=openai")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	p := &wireProcess{cmd: cmd, input: in, frames: make(chan frame, 64), diagnostics: stderr, done: make(chan error, 1)}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 64<<20)
		for scanner.Scan() {
			var f frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				p.frames <- frame{Error: json.RawMessage(`{"message":"non-JSON stdout"}`)}
				break
			}
			p.frames <- f
		}
		close(p.frames)
	}()
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = in.Close()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}
func (p *wireProcess) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.input.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}
func (p *wireProcess) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	p.next++
	id := p.next
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		select {
		case f, ok := <-p.frames:
			if !ok {
				t.Fatal("agent stdout closed")
			}
			if len(f.Error) > 0 {
				t.Fatalf("%s: %s", method, f.Error)
			}
			if f.Method == "session/update" {
				p.updates = append(p.updates, f.Params)
				continue
			}
			if f.Method == "session/request_permission" {
				p.send(t, map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}})
				continue
			}
			if string(f.ID) == fmt.Sprint(id) {
				return f.Result
			}
		case <-time.After(40 * time.Second):
			t.Fatalf("timeout waiting for %s", method)
		}
	}
}
func (p *wireProcess) stop(t *testing.T) {
	t.Helper()
	_ = p.input.Close()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("agent failed: %v; stderr: %s", err, p.diagnostics.String())
		}
		p.done <- nil
	case <-time.After(15 * time.Second):
		t.Fatal("agent did not stop after EOF")
	}
}

func TestExecutableToolPermissionAndPersistentHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "deerflow-acp-go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-o", bin, "./cmd/deerflow-acp-go")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		call := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			chunk := map[string]any{"id": "fixture", "object": "chat.completion.chunk", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "write-1", "type": "function", "function": map[string]any{"name": "write_file", "arguments": `{"path":"result.txt","content":"persisted by the real tool"}`}}}}, "finish_reason": nil}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Task complete.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	state, workspace := t.TempDir(), t.TempDir()
	p := launch(t, bin, state, server.URL+"/v1")
	initialize := map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}
	p.request(t, "initialize", initialize)
	created := p.request(t, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.ID == "" {
		t.Fatalf("new: %s", created)
	}
	response := p.request(t, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "Write the result file."}}})
	if !strings.Contains(string(response), "end_turn") {
		t.Fatalf("prompt: %s", response)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "result.txt"))
	if err != nil || string(data) != "persisted by the real tool" {
		t.Fatalf("tool result %q: %v", data, err)
	}
	var sawTool, sawText bool
	for _, raw := range p.updates {
		sawTool = sawTool || strings.Contains(string(raw), "tool_call")
		sawText = sawText || strings.Contains(string(raw), "Task complete.")
	}
	if !sawTool || !sawText {
		t.Fatalf("missing updates: tool=%v text=%v", sawTool, sawText)
	}
	p.stop(t)
	q := launch(t, bin, state, server.URL+"/v1")
	q.request(t, "initialize", initialize)
	q.request(t, "session/load", map[string]any{"sessionId": session.ID, "cwd": workspace, "mcpServers": []any{}})
	if len(q.updates) == 0 {
		t.Fatal("load responded before replaying history")
	}
	q.request(t, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]string{"type": "text", "text": "What did you write?"}}})
	mu.Lock()
	snapshot, _ := json.Marshal(requests[len(requests)-1])
	mu.Unlock()
	if !strings.Contains(string(snapshot), "Write the result file.") || !strings.Contains(string(snapshot), "persisted by the real tool") {
		t.Fatalf("native Eino history was not rebuilt: %s", snapshot)
	}
	q.stop(t)
}
