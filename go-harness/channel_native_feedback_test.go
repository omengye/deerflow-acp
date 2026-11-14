package deerflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
	"github.com/omengye/deerflow-acp/go-harness/internal/wechat"
)

// A real stdio plugin records only this test's synthetic messages. It makes the
// model-to-channel-to-plugin boundary observable without a live WeChat account.
type nativeFeedbackFixture struct{ dir string }

func (p nativeFeedbackFixture) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		d := wechat.Descriptor()
		d.ID, d.Name, d.IndependentPoll = "fixture", "Fixture", false
		return d, nil
	case "login.start":
		return channelproto.Login{State: "confirmed", Auth: &channelproto.Auth{Account: "bot", Owner: "alice", Secret: json.RawMessage(`{"token":"fixture"}`)}}, nil
	case "poll":
		var in channelproto.Poll
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(in.Cursor)
		body, err := os.ReadFile(filepath.Join(p.dir, fmt.Sprintf("in-%d.json", n+1)))
		if errors.Is(err, os.ErrNotExist) {
			return channelproto.Batch{Cursor: in.Cursor}, nil
		}
		var msg channelproto.Message
		if err != nil || json.Unmarshal(body, &msg) != nil {
			return nil, errors.New("invalid fixture inbox")
		}
		return channelproto.Batch{Cursor: strconv.Itoa(n + 1), Messages: []channelproto.Message{msg}}, nil
	case "typing.prepare":
		return channelproto.TypingResult{Ticket: "fixture-ticket"}, nil
	case "progress", "typing", "send":
		name := method + "-" + rand.Text() + ".json"
		if method == "send" {
			var in channelproto.Send
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, err
			}
			name = "reply-" + in.Message.ID + ".json"
			if strings.HasSuffix(in.Key, "-image-notice") {
				name = "image-notice-" + in.Message.ID + ".json"
			}
			if in.Image != nil {
				data, err := os.ReadFile(in.Image.Path)
				if err != nil || int64(len(data)) != in.Image.Size || fmt.Sprintf("%x", sha256.Sum256(data)) != in.Image.SHA256 {
					return nil, errors.New("invalid fixture image snapshot")
				}
				name = "image-" + in.Message.ID + "-" + in.Key + ".json"
			}
		}
		if err := os.WriteFile(filepath.Join(p.dir, name), raw, 0600); err != nil {
			return nil, err
		}
		if method == "progress" {
			if _, err := os.Stat(filepath.Join(p.dir, "hang-progress")); err == nil {
				time.Sleep(30 * time.Second) // the host must kill this process
			}
			if _, err := os.Stat(filepath.Join(p.dir, "fail-progress")); err == nil {
				return nil, errors.New("PRIVATE-PROVIDER-ERROR")
			}
		}
		return map[string]bool{"sent": true}, nil
	default:
		return nil, errors.New("unexpected fixture operation")
	}
}

func TestNativeFeedbackFixtureChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "native-feedback-fixture" {
		return
	}
	if err := channelproto.Serve(context.Background(), os.Stdin, os.Stdout, nativeFeedbackFixture{os.Args[len(os.Args)-1]}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func setupNativeFeedbackFixture(t *testing.T, c *Client, workspace, dir, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	manifest := channels.Manifest{ID: "fixture", Command: exe, Args: []string{"-test.run=^TestNativeFeedbackFixtureChild$", "native-feedback-fixture", dir}, SHA256: hex.EncodeToString(sum[:])}
	ctx := context.Background()
	if _, err = c.channels.Install(ctx, "native-test", manifest); err != nil {
		t.Fatal(err)
	}
	if _, err = c.channels.Configure(ctx, "native-test", 1, workspace, map[string]string{"progress_mode": mode}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = c.channels.Login(ctx, "native-test", "login.start", "", ""); err != nil {
		t.Fatal(err)
	}
}

func putFeedbackMessage(t *testing.T, dir string, n int, text string) {
	t.Helper()
	msg := channelproto.Message{ID: strconv.Itoa(n), Sender: "alice", Conversation: "alice", Context: "fixture-context", Text: text}
	body, _ := json.Marshal(msg)
	target := filepath.Join(dir, fmt.Sprintf("in-%d.json", n))
	if err := os.WriteFile(target+".tmp", body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target+".tmp", target); err != nil {
		t.Fatal(err)
	}
}

func waitFeedbackFile(t *testing.T, dir, pattern string, check func([]byte) bool) []byte {
	t.Helper()
	body, err := awaitFeedbackFile(t.Context(), dir, pattern, check)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func awaitFeedbackFile(ctx context.Context, dir, pattern string, check func([]byte) bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		paths, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err == nil && json.Valid(body) && check(body) {
				return body, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %s: %w", pattern, ctx.Err())
		case <-ticker.C:
		}
	}
}

func feedbackReply(t *testing.T, dir string, n int) channelproto.Send {
	t.Helper()
	body := waitFeedbackFile(t, dir, fmt.Sprintf("reply-%d.json", n), func([]byte) bool { return true })
	var reply channelproto.Send
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestChannelNativeFeedbackAndTextFallbackThroughPlugin(t *testing.T) {
	for _, mode := range []string{"native", "unknown_tool", "failed", "timeout", "summary"} {
		t.Run(mode, func(t *testing.T) {
			dir, workspace := t.TempDir(), testutil.CanonicalTempDir(t)
			nativeSuccess := mode == "native" || mode == "unknown_tool"
			toolName := "read_file"
			if mode == "unknown_tool" {
				toolName = "PRIVATE-UNKNOWN-TOOL"
			}
			var calls atomic.Int32
			cfg := Config{DataDir: t.TempDir(), Engine: engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				calls.Add(1)
				for _, kind := range []string{"tool_start", "tool_execute"} {
					status := "pending"
					if kind == "tool_execute" {
						status = "in_progress"
					}
					if err := emit(ctx, harness.RunEvent{Kind: kind, ToolCallID: "PRIVATE-ID", ToolName: toolName, Status: status, Arguments: json.RawMessage(`{"path":"PRIVATE-ARGUMENT"}`)}); err != nil {
						return harness.RunResult{}, err
					}
				}
				// The model cannot finish until feedback reaches the plugin: this
				// detects accidental end-of-turn buffering. Summary mode has no
				// native progress, so it waits for the typing indicator instead;
				// an answer ready before the plugin starts intentionally skips
				// typing, which made a fast engine race the plugin process start.
				pattern, marker := "progress-*.json", `"phase":"start"`
				if mode == "summary" {
					pattern, marker = "typing-*.json", `"active":true`
				}
				if _, err := awaitFeedbackFile(ctx, dir, pattern, func(body []byte) bool { return strings.Contains(string(body), marker) }); err != nil {
					return harness.RunResult{}, err
				}
				if err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: "PRIVATE-ID", ToolName: toolName, Status: "completed", Content: []harness.Content{{Type: "text", Text: "PRIVATE-OUTPUT"}}}); err != nil {
					return harness.RunResult{}, err
				}
				return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "final answer"})
			})}
			c, err := Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if mode == "failed" {
				if err := os.WriteFile(filepath.Join(dir, "fail-progress"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "timeout" {
				if err := os.WriteFile(filepath.Join(dir, "hang-progress"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			setting := "native"
			if mode == "summary" {
				setting = "summary"
			}
			setupNativeFeedbackFixture(t, c, workspace, dir, setting)
			putFeedbackMessage(t, dir, 1, "read a file")
			reply := feedbackReply(t, dir, 1)
			if calls.Load() != 1 || !strings.Contains(reply.Text, "final answer") || strings.Contains(reply.Text, "工具调用摘要") == nativeSuccess || strings.Contains(reply.Text, "PRIVATE") {
				t.Fatal("wrong final/fallback reply", reply.Text, calls.Load())
			}
			waitFeedbackFile(t, dir, "typing-*.json", func(body []byte) bool { return strings.Contains(string(body), `"active":false`) })
			paths, _ := filepath.Glob(filepath.Join(dir, "progress-*.json"))
			if mode == "summary" && len(paths) != 0 || nativeSuccess && len(paths) != 2 {
				t.Fatal("duplicate or unwanted native progress", paths)
			}
			for _, path := range paths {
				body, _ := os.ReadFile(path)
				label := "read_file"
				if mode == "unknown_tool" {
					label = "扩展工具"
				}
				if strings.Contains(string(body), "PRIVATE") || !strings.Contains(string(body), label) {
					t.Fatal("unsafe or missing tool label", string(body))
				}
			}
		})
	}
}

func TestChannelNativeFeedbackOnPermissionWaitAndResume(t *testing.T) {
	dir := t.TempDir()
	workspace, cfg := skillFixture(t)
	cfg.PermissionMode = harness.PermissionModeAll
	var toolCalls, modelCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		toolCalls.Add(1)
		w.Write([]byte("fixture result"))
	}))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		var req struct {
			Stream   bool
			Messages []struct{ Role string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		for _, msg := range req.Messages {
			if msg.Role == "tool" {
				skillToolResponse(w, req.Stream, "", "", "resumed answer")
				return
			}
		}
		args, _ := json.Marshal(map[string]string{"url": target.URL})
		skillToolResponse(w, req.Stream, "web_fetch", string(args), "")
	}))
	defer provider.Close()
	cfg.BuiltinTools = []harness.BuiltinToolConfig{{Name: "web_fetch"}}
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	setupNativeFeedbackFixture(t, c, workspace, dir, "native")
	putFeedbackMessage(t, dir, 1, "fetch")
	first := feedbackReply(t, dir, 1)
	code := regexp.MustCompile(`/approve ([A-Z2-7]{12})`).FindStringSubmatch(first.Text)
	if len(code) != 2 || toolCalls.Load() != 0 {
		t.Fatal("permission notice or execution boundary lost", first.Text)
	}
	waitFeedbackFile(t, dir, "progress-*.json", func(body []byte) bool { return strings.Contains(string(body), `"status":"blocked"`) })
	putFeedbackMessage(t, dir, 2, "/approve "+code[1])
	final := feedbackReply(t, dir, 2)
	if !strings.Contains(final.Text, "resumed answer") || strings.Contains(final.Text, "工具调用摘要") || toolCalls.Load() != 1 || modelCalls.Load() != 2 {
		t.Fatal("resume lost progress, answer or replay protection", final.Text, toolCalls.Load(), modelCalls.Load())
	}
	waitFeedbackFile(t, dir, "progress-*.json", func(body []byte) bool { return strings.Contains(string(body), `"status":"completed"`) })
	paths, _ := filepath.Glob(filepath.Join(dir, "progress-*.json"))
	if len(paths) != 4 {
		t.Fatal("expected start/blocked and resumed start/completed", len(paths))
	}
}
