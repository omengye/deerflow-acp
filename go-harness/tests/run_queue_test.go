package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func queueResponse(t *testing.T, p *wireProcess, id int, timeout time.Duration) frame {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case message, ok := <-p.frames:
			if !ok {
				t.Fatalf("ACP process closed while waiting for %d: %s", id, p.diagnostics.String())
			}
			if message.Method == "session/update" {
				continue
			}
			if message.Method != "" || string(message.ID) != strconv.Itoa(id) {
				t.Fatalf("unexpected ACP frame while waiting for %d: %+v", id, message)
			}
			return message
		case <-deadline.C:
			t.Fatalf("timed out waiting for ACP response %d: %s", id, p.diagnostics.String())
		}
	}
}

func queueArrival(t *testing.T, arrivals <-chan string) string {
	t.Helper()
	select {
	case marker := <-arrivals:
		return marker
	case <-time.After(10 * time.Second):
		t.Fatal("model fixture did not receive the prompt")
		return ""
	}
}

func TestExecutableRunQueueAcrossACPSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "deerflow-acp-go")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-o", binary, "./cmd/deerflow-acp-go")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	arrivals := make(chan string, 4)
	release := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		marker := "unknown"
		for _, name := range []string{"first-prompt", "second-prompt", "third-prompt"} {
			if bytes.Contains(body, []byte(name)) {
				marker = name
				break
			}
		}
		arrivals <- marker
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"queue-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"queued completion\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"queue-fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defer close(release)
	p := launch(t, binary, t.TempDir(), server.URL+"/v1", "--max-active-runs", "2", "--queue-timeout", "750ms", "--disable-subagents")
	p.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	workspace := t.TempDir()
	sessions := make([]string, 3)
	for i := range sessions {
		raw := p.request(t, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &created); err != nil || created.SessionID == "" {
			t.Fatalf("new session %d: %s: %v", i, raw, err)
		}
		sessions[i] = created.SessionID
	}
	prompt := func(id int, sessionID, marker string) {
		p.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": map[string]any{
			"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": marker}},
		}})
	}
	prompt(201, sessions[0], "first-prompt")
	prompt(202, sessions[1], "second-prompt")
	seen := map[string]bool{queueArrival(t, arrivals): true, queueArrival(t, arrivals): true}
	if !seen["first-prompt"] || !seen["second-prompt"] {
		t.Fatalf("first two model calls were not admitted: %+v", seen)
	}
	prompt(203, sessions[2], "third-prompt")
	select {
	case marker := <-arrivals:
		t.Fatalf("third model call bypassed both occupied slots: %s", marker)
	case <-time.After(100 * time.Millisecond):
	}
	if response := queueResponse(t, p, 203, 5*time.Second); !strings.Contains(string(response.Error), "Run queue timeout") {
		t.Fatalf("queued prompt did not time out with ACP ServerBusy: %+v", response)
	}
	// A cancelled queue wait must not start later when the occupied slots open.
	prompt(204, sessions[2], "third-prompt")
	p.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessions[2]}})
	if response := queueResponse(t, p, 204, 5*time.Second); !strings.Contains(string(response.Result), "cancelled") || len(response.Error) != 0 {
		t.Fatalf("queued cancellation result: %+v", response)
	}
	release <- struct{}{}
	release <- struct{}{}
	completed := make(map[string]bool)
	for len(completed) < 2 {
		select {
		case response, ok := <-p.frames:
			if !ok {
				t.Fatalf("ACP process closed before active prompts completed: %s", p.diagnostics.String())
			}
			if response.Method == "session/update" {
				continue
			}
			id := string(response.ID)
			if (id != "201" && id != "202") || completed[id] || len(response.Error) != 0 || !strings.Contains(string(response.Result), "end_turn") {
				t.Fatalf("active prompt %s did not complete: %+v", id, response)
			}
			completed[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("active ACP prompt timed out: completed=%+v", completed)
		}
	}
	select {
	case marker := <-arrivals:
		t.Fatalf("timed out or cancelled prompt reached the model later: %s", marker)
	case <-time.After(100 * time.Millisecond):
	}
	prompt(205, sessions[2], "third-prompt")
	if marker := queueArrival(t, arrivals); marker != "third-prompt" {
		t.Fatalf("retry reached model as %q", marker)
	}
	release <- struct{}{}
	response := queueResponse(t, p, 205, 5*time.Second)
	if len(response.Error) != 0 || !strings.Contains(string(response.Result), "end_turn") {
		t.Fatalf("retry failed after slot release: %+v", response)
	}
	p.stop(t)
}
