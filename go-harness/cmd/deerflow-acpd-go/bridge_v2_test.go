package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type bridgeV2Frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type bridgeV2Client struct {
	cmd         *exec.Cmd
	input       io.WriteCloser
	frames      chan bridgeV2Frame
	done        chan error
	stderr      bytes.Buffer
	nextID      int
	updates     []json.RawMessage
	seen        []json.RawMessage
	approve     bool
	permissions int
}

func launchBridgeV2(t *testing.T, bridge, runtimeDir string) *bridgeV2Client {
	t.Helper()
	client := &bridgeV2Client{frames: make(chan bridgeV2Frame, 128), done: make(chan error, 1)}
	client.cmd = exec.Command(bridge, "--protocol", "v2", "--no-auto-start", "--runtime-dir", runtimeDir)
	var err error
	client.input, err = client.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := client.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	client.cmd.Stderr = &client.stderr
	if err := client.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 64<<20)
		for scanner.Scan() {
			var frame bridgeV2Frame
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				client.frames <- bridgeV2Frame{Error: json.RawMessage(`{"message":"invalid JSON from Bridge"}`)}
				break
			}
			client.frames <- frame
		}
		close(client.frames)
	}()
	go func() { client.done <- client.cmd.Wait() }()
	t.Cleanup(func() {
		_ = client.input.Close()
		select {
		case <-client.done:
		case <-time.After(5 * time.Second):
			_ = client.cmd.Process.Kill()
			<-client.done
		}
	})
	return client
}

func (c *bridgeV2Client) send(t *testing.T, value any) {
	t.Helper()
	if err := json.NewEncoder(c.input).Encode(value); err != nil {
		t.Fatalf("Bridge send: %v; %s", err, c.stderr.String())
	}
}

func (c *bridgeV2Client) next(t *testing.T) bridgeV2Frame {
	t.Helper()
	select {
	case frame, ok := <-c.frames:
		if !ok {
			t.Fatalf("Bridge closed stdout: %s", c.stderr.String())
		}
		if frame.Method == "session/update" {
			c.updates = append(c.updates, frame.Params)
			c.seen = append(c.seen, frame.Params)
		}
		if frame.Method == "session/request_permission" {
			c.permissions++
			if !c.approve {
				t.Fatalf("unexpected permission request: %s", frame.Params)
			}
			var request struct {
				Options []struct {
					OptionID string `json:"optionId"`
					Kind     string `json:"kind"`
				} `json:"options"`
			}
			if err := json.Unmarshal(frame.Params, &request); err != nil {
				t.Fatal(err)
			}
			for _, option := range request.Options {
				if option.Kind == "allow_once" {
					c.send(t, map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option.OptionID}}})
					return frame
				}
			}
			t.Fatalf("permission request has no allow_once option: %s", frame.Params)
		}
		return frame
	case <-time.After(20 * time.Second):
		t.Fatalf("Bridge response timeout: %s", c.stderr.String())
		return bridgeV2Frame{}
	}
}

func (c *bridgeV2Client) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	c.nextID++
	id := c.nextID
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	for {
		frame := c.next(t)
		if string(frame.ID) != fmt.Sprint(id) {
			if len(frame.ID) != 0 && frame.Method == "" {
				t.Fatalf("unexpected response while waiting for %s: %+v", method, frame)
			}
			continue
		}
		if len(frame.Error) != 0 {
			t.Fatalf("Bridge %s: %s; %s", method, frame.Error, c.stderr.String())
		}
		return frame.Result
	}
}

func (c *bridgeV2Client) waitState(t *testing.T, sessionID, state string) map[string]any {
	t.Helper()
	for {
		for len(c.updates) > 0 {
			raw := c.updates[0]
			c.updates = c.updates[1:]
			var event struct {
				SessionID string         `json:"sessionId"`
				Update    map[string]any `json:"update"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.SessionID == sessionID && event.Update["sessionUpdate"] == "state_update" && event.Update["state"] == state {
				return event.Update
			}
		}
		frame := c.next(t)
		if len(frame.Error) != 0 {
			t.Fatalf("Bridge frame error: %s", frame.Error)
		}
	}
}

func (c *bridgeV2Client) close(t *testing.T) {
	t.Helper()
	_ = c.input.Close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("Bridge exit: %v; %s", err, c.stderr.String())
		}
		c.done <- nil
	case <-time.After(10 * time.Second):
		t.Fatalf("Bridge did not exit after EOF: %s", c.stderr.String())
	}
}

func (c *bridgeV2Client) assertUpdateSession(t *testing.T, start int, sessionID string) {
	t.Helper()
	if len(c.seen) == start {
		t.Fatal("prompt emitted no session updates")
	}
	for _, raw := range c.seen[start:] {
		var event struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &event); err != nil || event.SessionID != sessionID {
			t.Fatalf("cross-session update while prompting %s: %s %v", sessionID, raw, err)
		}
	}
}

// This is an optional cross-language test. Set DEERFLOW_TEST_BRIDGE to a built
// native Rust binary; the test still launches the real Go daemon and Eino model
// adapter, using only a local model fixture.
func TestExistingRustBridgeV2Lifecycle(t *testing.T) {
	bridge := os.Getenv("DEERFLOW_TEST_BRIDGE")
	if bridge == "" || testing.Short() {
		t.Skip("set DEERFLOW_TEST_BRIDGE to a native Bridge executable")
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	encodedImage := base64.StdEncoding.EncodeToString(pngData.Bytes())
	var imageReachedModel atomic.Bool
	modelStarted := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if bytes.Contains(body, []byte("data:image/png;base64,"+encodedImage)) {
			imageReachedModel.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if bytes.Contains(body, []byte("wait-for-cancel")) {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Working.\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
			modelStarted <- struct{}{}
			<-r.Context().Done()
			return
		}
		if bytes.Contains(body, []byte("write-permission")) && !bytes.Contains(body, []byte(`"role":"tool"`)) {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Preparing.\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"write-v2\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"v2-result.txt\\\",\\\"content\\\":\\\"approved\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done.\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	dataDir, runtimeDir, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	daemon := launchDaemon(t, dataDir, runtimeDir, provider.URL+"/v1", "--vision-model", "fixture-model")
	ep := daemon.endpoint(t, runtimeDir)
	client := launchBridgeV2(t, bridge, runtimeDir)
	initialized := client.request(t, "initialize", map[string]any{"protocolVersion": 2, "capabilities": map[string]any{}, "info": map[string]string{"name": "go-v2-test", "version": "1"}})
	if !bytes.Contains(initialized, []byte(`"protocolVersion":2`)) {
		t.Fatalf("initialize: %s", initialized)
	}
	var capabilities struct {
		Capabilities struct {
			Session struct {
				Prompt map[string]json.RawMessage `json:"prompt"`
			} `json:"session"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(initialized, &capabilities); err != nil {
		t.Fatal(err)
	}
	if _, ok := capabilities.Capabilities.Session.Prompt["image"]; !ok {
		t.Fatalf("v2 image capability missing: %s", initialized)
	}
	create := func() string {
		t.Helper()
		raw := client.request(t, "session/new", map[string]any{"cwd": workspace, "additionalDirectories": []any{}, "mcpServers": []any{}})
		var value struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || value.SessionID == "" {
			t.Fatalf("session/new: %s %v", raw, err)
		}
		client.waitState(t, value.SessionID, "idle")
		return value.SessionID
	}
	first, second := create(), create()
	listed := client.request(t, "session/list", map[string]any{"cwd": workspace})
	if !bytes.Contains(listed, []byte(first)) || !bytes.Contains(listed, []byte(second)) {
		t.Fatalf("session/list: %s", listed)
	}
	firstStart := len(client.seen)
	ack := client.request(t, "session/prompt", map[string]any{"sessionId": first, "prompt": []any{map[string]string{"type": "text", "text": "hello"}}})
	if string(ack) != "{}" {
		t.Fatalf("v2 prompt ACK: %s", ack)
	}
	if len(client.seen) != firstStart {
		t.Fatal("v2 prompt emitted updates before ACK")
	}
	client.waitState(t, first, "running")
	if idle := client.waitState(t, first, "idle"); idle["stopReason"] != "end_turn" {
		t.Fatalf("first prompt idle: %+v", idle)
	}
	client.assertUpdateSession(t, firstStart, first)
	var liveText bool
	for _, raw := range client.seen[firstStart:] {
		if bytes.Contains(raw, []byte("Done.")) {
			liveText = true
		}
	}
	if !liveText {
		t.Fatal("v2 facade lost live assistant text")
	}
	client.request(t, "session/prompt", map[string]any{"sessionId": first, "prompt": []any{map[string]string{"type": "text", "text": "inspect image"}, map[string]string{"type": "image", "data": encodedImage, "mimeType": "image/png"}}})
	client.waitState(t, first, "running")
	if idle := client.waitState(t, first, "idle"); idle["stopReason"] != "end_turn" {
		t.Fatalf("image prompt idle: %+v", idle)
	}
	if !imageReachedModel.Load() {
		t.Fatal("v2 image was not delivered to the Eino model adapter")
	}
	client.approve = true
	permissionStart := len(client.seen)
	client.request(t, "session/prompt", map[string]any{"sessionId": first, "prompt": []any{map[string]string{"type": "text", "text": "write-permission"}}})
	client.waitState(t, first, "running")
	if idle := client.waitState(t, first, "idle"); idle["stopReason"] != "end_turn" {
		var updates []string
		for _, raw := range client.seen[permissionStart:] {
			updates = append(updates, string(raw))
		}
		t.Fatalf("permission prompt idle: %+v updates=%s", idle, strings.Join(updates, "\n"))
	}
	client.assertUpdateSession(t, permissionStart, first)
	var beforeToolID, afterToolID string
	for _, raw := range client.seen[permissionStart:] {
		var event struct {
			Update struct {
				MessageID string          `json:"messageId"`
				Content   json.RawMessage `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Update.Content, &content) != nil {
			continue
		}
		switch content.Text {
		case "Preparing.":
			beforeToolID = event.Update.MessageID
		case "Done.":
			afterToolID = event.Update.MessageID
		}
	}
	if beforeToolID == "" || afterToolID == "" || beforeToolID == afterToolID {
		var updates []string
		for _, raw := range client.seen[permissionStart:] {
			updates = append(updates, string(raw))
		}
		t.Fatalf("tool-separated assistant messages lack distinct IDs: before=%q after=%q updates=%s", beforeToolID, afterToolID, strings.Join(updates, "\n"))
	}
	if client.permissions != 1 {
		t.Fatalf("v2 permission requests: %d", client.permissions)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, "v2-result.txt")); err != nil || string(content) != "approved" {
		t.Fatalf("approved tool side effect: %q %v", content, err)
	}
	secondStart := len(client.seen)
	ack = client.request(t, "session/prompt", map[string]any{"sessionId": second, "prompt": []any{map[string]string{"type": "text", "text": "wait-for-cancel"}}})
	if string(ack) != "{}" {
		t.Fatalf("second prompt ACK: %s", ack)
	}
	client.waitState(t, second, "running")
	select {
	case <-modelStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation fixture model did not start")
	}
	client.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": second}})
	if idle := client.waitState(t, second, "idle"); idle["stopReason"] != "cancelled" {
		t.Fatalf("cancel idle: %+v", idle)
	}
	client.assertUpdateSession(t, secondStart, second)
	client.close(t)

	resumed := launchBridgeV2(t, bridge, runtimeDir)
	resumed.request(t, "initialize", map[string]any{"protocolVersion": 2, "capabilities": map[string]any{}, "info": map[string]string{"name": "go-v2-resume-test", "version": "1"}})
	resumed.request(t, "session/resume", map[string]any{"sessionId": first, "cwd": workspace, "additionalDirectories": []any{}, "mcpServers": []any{}, "replayFrom": map[string]string{"type": "start"}})
	resumed.waitState(t, first, "idle")
	var replayed bool
	for _, raw := range resumed.seen {
		if bytes.Contains(raw, []byte("Done.")) {
			replayed = true
		}
	}
	if !replayed {
		var rendered []string
		for _, raw := range resumed.seen {
			rendered = append(rendered, string(raw))
		}
		t.Fatalf("resume from start did not replay completed prompt: %s; Bridge stderr: %s", strings.Join(rendered, "\n"), resumed.stderr.String())
	}
	var replayedBefore, replayedAfter bool
	for _, raw := range resumed.seen {
		var event struct {
			Update struct {
				MessageID string          `json:"messageId"`
				Content   json.RawMessage `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Update.Content, &content) != nil {
			continue
		}
		replayedBefore = replayedBefore || (content.Text == "Preparing." && event.Update.MessageID == beforeToolID)
		replayedAfter = replayedAfter || (content.Text == "Done." && event.Update.MessageID == afterToolID)
	}
	if !replayedBefore || !replayedAfter {
		t.Fatalf("tool-separated message IDs changed on replay: before=%v after=%v", replayedBefore, replayedAfter)
	}
	resumed.request(t, "session/close", map[string]any{"sessionId": first})
	beforeResume := len(resumed.seen)
	resumed.request(t, "session/resume", map[string]any{"sessionId": second, "cwd": workspace, "additionalDirectories": []any{}, "mcpServers": []any{}})
	resumed.waitState(t, second, "idle")
	if len(resumed.seen) != beforeResume+1 {
		t.Fatalf("resume without replay emitted historical updates: %d", len(resumed.seen)-beforeResume)
	}
	resumed.request(t, "session/close", map[string]any{"sessionId": second})
	resumed.close(t)

	conn, _, line := connectCommand(t, ep, "STOP")
	_ = conn.Close()
	if line != "OK" {
		t.Fatal(line)
	}
	daemon.wait(t, true)
	if _, err := os.Stat(filepath.Join(runtimeDir, "endpoint.json")); !os.IsNotExist(err) {
		t.Fatalf("endpoint after stop: %v", err)
	}
	if strings.Contains(client.stderr.String(), ep.Token) || strings.Contains(resumed.stderr.String(), ep.Token) {
		t.Fatal("Bridge leaked daemon token")
	}
	if strings.Contains(client.stderr.String(), "skipped unrepresentable") || strings.Contains(resumed.stderr.String(), "skipped unrepresentable") {
		t.Fatalf("Bridge dropped a v1 update during v2 conversion: %s %s", client.stderr.String(), resumed.stderr.String())
	}
}
