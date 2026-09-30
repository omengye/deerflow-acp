package client

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestExternalACPProcess(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	config := Config{Command: executable, Args: []string{"-test.run=^TestExternalACPProcess$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1"}, Timeout: 5 * time.Second}
	var updates []string
	result, err := Run(context.Background(), config, workspace, "", "first", Callbacks{Update: func(_ context.Context, raw json.RawMessage) error {
		updates = append(updates, string(raw))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "child-1" || result.Text != "hello" || result.StopReason != "end_turn" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(updates) != 2 || !strings.Contains(updates[1], "tool_call") {
		t.Fatalf("updates not forwarded: %v", updates)
	}
	result, err = Run(context.Background(), config, workspace, result.SessionID, "second", Callbacks{Permission: func(_ context.Context, request PermissionRequest) (string, error) {
		if request.SessionID != "child-1" {
			t.Fatalf("permission on %q", request.SessionID)
		}
		return "allow_once", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello" {
		t.Fatalf("load/prompt: %+v", result)
	}
	_, err = Run(context.Background(), Config{Command: "relative", Timeout: time.Second}, workspace, "", "first", Callbacks{})
	if err == nil {
		t.Fatal("relative executable accepted")
	}
	if err = os.WriteFile(filepath.Join(workspace, "ordinary"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), config, filepath.Join(workspace, "ordinary"), "", "first", Callbacks{})
	if err == nil {
		t.Fatal("file workspace accepted")
	}
}

func TestExternalACPCancel(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = Run(ctx, Config{Command: executable, Args: []string{"-test.run=^TestExternalACPCancel$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1"}}, t.TempDir(), "", "hang", Callbacks{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("agent was not reaped promptly")
	}
}

func TestExternalResourceLinkBoundary(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "report.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	fileURI := func(path string) string {
		value := filepath.ToSlash(path)
		if runtime.GOOS == "windows" {
			value = "/" + value
		}
		return (&url.URL{Scheme: "file", Path: value}).String()
	}
	if err := safeResourceLink(workspace, fileURI(inside)); err != nil {
		t.Fatal(err)
	}
	if err := safeResourceLink(workspace, fileURI(outside)); err == nil {
		t.Fatal("outside file resource accepted")
	}
	if err := safeResourceLink(workspace, "https://example.test/report"); err != nil {
		t.Fatal(err)
	}
	if err := safeResourceLink(workspace, "data:text/plain,private"); err == nil {
		t.Fatal("inline resource accepted")
	}
}

func TestInvokeToolPersistsRemoteSession(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	identity := sha256.Sum256([]byte("parent-1"))
	stateFile := filepath.Join(root, "acp-agent-sessions", fmt.Sprintf("%x", identity[:]), "fixture.json")
	tool, err := Tool(root, harness.Session{ID: "parent-1"}, map[string]harness.ACPAgentConfig{"fixture": {Command: executable, Args: []string{"-test.run=^TestInvokeToolPersistsRemoteSession$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1", "DEERFLOW_ACP_STATE_FILE": stateFile}, TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first", "second"} {
		output, err := tool.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"`+prompt+`"}`)
		if err != nil || output != "hello" {
			t.Fatalf("%s: %q %v", prompt, output, err)
		}
	}
	_, err = tool.InvokableRun(context.Background(), `{"agent":"unknown","prompt":"task"}`)
	if err == nil {
		t.Fatal("unconfigured agent accepted")
	}
	if err := CleanupSession(root, "parent-1"); err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"acp-workspaces", "acp-agent-sessions"} {
		if _, err := os.Stat(filepath.Join(root, category, fmt.Sprintf("%x", identity[:]))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived cleanup: %v", category, err)
		}
	}
}

func TestInvokeToolWaitsForCommittedReceiptBeforeNextPrompt(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tool, err := Tool(root, harness.Session{ID: "parent-ack"}, map[string]harness.ACPAgentConfig{"fixture": {Command: executable, Args: []string{"-test.run=^TestInvokeToolWaitsForCommittedReceiptBeforeNextPrompt$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1"}, TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	var complete func(context.Context) error
	ctx := WithCallbacks(context.Background(), Callbacks{RegisterCompletion: func(fn func(context.Context) error) { complete = fn }})
	output, err := tool.InvokableRun(ctx, `{"agent":"fixture","prompt":"first"}`)
	if err != nil || output != "hello" || complete == nil {
		t.Fatalf("first prompt=%q err=%v completion=%t", output, err, complete != nil)
	}
	identity := sha256.Sum256([]byte("parent-ack"))
	stateFile := filepath.Join(root, "acp-agent-sessions", fmt.Sprintf("%x", identity[:]), "fixture.json")
	raw, err := readSessionState(stateFile)
	var state sessionState
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Pending == nil || state.Pending.ID == "" {
		t.Fatalf("unacknowledged prompt not pinned: %+v %v", state, err)
	}
	if _, err := tool.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"second"}`); !errors.Is(err, harness.ErrCommandUncertain) {
		t.Fatalf("new prompt bypassed unacknowledged remote result: %v", err)
	}
	if err := complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	output, err = tool.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"second"}`)
	if err != nil || output != "hello" {
		t.Fatalf("prompt after receipt acknowledgement=%q err=%v", output, err)
	}
}

func TestInvokeToolBlocksAfterRemotePromptBecomesUncertain(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tool, err := Tool(root, harness.Session{ID: "parent-uncertain"}, map[string]harness.ACPAgentConfig{"fixture": {Command: executable, Args: []string{"-test.run=^TestInvokeToolBlocksAfterRemotePromptBecomesUncertain$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1"}, TimeoutSeconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"hang"}`); !errors.Is(err, harness.ErrCommandUncertain) {
		t.Fatalf("timed-out remote prompt was not marked uncertain: %v", err)
	}
	if _, err := tool.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"retry"}`); !errors.Is(err, harness.ErrCommandUncertain) {
		t.Fatalf("uncertain prompt allowed a new remote invocation: %v", err)
	}
}

func TestExternalACPSessionPersistenceFailureStopsBeforePrompt(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "prompt-sent")
	sentinel := errors.New("state store unavailable")
	result, err := Run(context.Background(), Config{Command: executable, Args: []string{"-test.run=^TestExternalACPSessionPersistenceFailureStopsBeforePrompt$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1", "DEERFLOW_ACP_PROMPT_MARKER": marker}, Timeout: 5 * time.Second}, t.TempDir(), "", "first", Callbacks{SessionReady: func(_ context.Context, sessionID string) error {
		if sessionID != "child-1" {
			t.Errorf("remote session ID=%q", sessionID)
		}
		return sentinel
	}})
	if !errors.Is(err, sentinel) || result.SessionID != "" {
		t.Fatalf("unpersisted session dispatched: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("prompt reached remote agent before state commit: %v", statErr)
	}
}

func TestExternalACPPromptPersistenceFailureStopsBeforeDispatch(t *testing.T) {
	if os.Getenv("DEERFLOW_ACP_HELPER") != "" {
		helperAgent()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "prompt-sent")
	sentinel := errors.New("prompt journal unavailable")
	result, err := Run(context.Background(), Config{Command: executable, Args: []string{"-test.run=^TestExternalACPPromptPersistenceFailureStopsBeforeDispatch$"}, Env: map[string]string{"DEERFLOW_ACP_HELPER": "1", "DEERFLOW_ACP_PROMPT_MARKER": marker}, Timeout: 5 * time.Second}, t.TempDir(), "", "first", Callbacks{PromptReady: func(_ context.Context, sessionID string) error {
		if sessionID != "child-1" {
			t.Errorf("unexpected remote session %q", sessionID)
		}
		return sentinel
	}})
	if !errors.Is(err, sentinel) || result.PromptDispatched {
		t.Fatalf("unpersisted prompt dispatched: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("prompt reached remote agent before journal commit: %v", statErr)
	}
}

func helperAgent() {
	writer := bufio.NewWriter(os.Stdout)
	reader := bufio.NewScanner(os.Stdin)
	write := func(value any) { _ = json.NewEncoder(writer).Encode(value); _ = writer.Flush() }
	for reader.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(reader.Bytes(), &frame) != nil {
			os.Exit(2)
		}
		if len(frame.ID) > 0 && frame.Method == "" {
			continue
		}
		switch frame.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}}})
		case "session/new":
			write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{"sessionId": "child-1"}})
		case "session/load":
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "child-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "old history"}}}})
			write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]any{}})
		case "session/prompt":
			if stateFile := os.Getenv("DEERFLOW_ACP_STATE_FILE"); stateFile != "" {
				raw, err := os.ReadFile(stateFile)
				var state sessionState
				if err != nil || json.Unmarshal(raw, &state) != nil || state.SessionID != "child-1" || state.Policy == "" {
					os.Exit(6)
				}
			}
			if marker := os.Getenv("DEERFLOW_ACP_PROMPT_MARKER"); marker != "" {
				if os.WriteFile(marker, []byte("sent"), 0600) != nil {
					os.Exit(7)
				}
			}
			if strings.Contains(string(frame.Params), "hang") {
				continue
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "child-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "hello"}}}})
			write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "child-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "nested-1", "title": "read_file", "status": "pending"}}})
			write(map[string]any{"jsonrpc": "2.0", "id": "permission-1", "method": "session/request_permission", "params": map[string]any{"sessionId": "child-1", "toolCall": map[string]any{"toolCallId": "nested-1", "title": "read_file", "status": "pending"}, "options": []map[string]string{{"optionId": "allow_once", "kind": "allow_once"}, {"optionId": "reject_once", "kind": "reject_once"}}}})
			// The client may cancel by default or select an offered option.
			if !reader.Scan() {
				os.Exit(3)
			}
			var answer struct {
				ID     string `json:"id"`
				Result struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				} `json:"result"`
			}
			if json.Unmarshal(reader.Bytes(), &answer) != nil || answer.ID != "permission-1" || (answer.Result.Outcome.Outcome != "cancelled" && answer.Result.Outcome.OptionID != "allow_once") {
				os.Exit(4)
			}
			write(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": map[string]string{"stopReason": "end_turn"}})
		case "session/cancel":
			os.Exit(0)
		default:
			os.Exit(5)
		}
	}
}
