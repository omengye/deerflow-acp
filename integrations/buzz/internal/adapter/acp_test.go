package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// This subprocess fixture never contacts a relay or model provider.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("BUZZ_ADAPTER_TEST_HELPER")
	if mode == "" {
		return
	}
	if strings.HasPrefix(mode, "cli-") {
		switch mode {
		case "cli-timeout":
			time.Sleep(20 * time.Second)
		case "cli-overflow":
			fmt.Print(strings.Repeat("x", 1<<20))
			time.Sleep(20 * time.Second)
		case "cli-transport":
			fmt.Fprintln(os.Stderr, `{"error":"transport","retryable":true}`)
			os.Exit(1)
		case "cli-unknown":
			fmt.Fprintln(os.Stderr, `{"error":"delivery_unknown","retryable":true}`)
			os.Exit(1)
		case "cli-send":
			fmt.Printf(`{"accepted":true,"event_id":"%s"}`, strings.Repeat("a", 64))
		}
		os.Exit(0)
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	send := func(id json.RawMessage, result any) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	update := func(value map[string]any) {
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session", "update": value}})
	}
	for scanner.Scan() {
		var m envelope
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(2)
		}
		switch m.Method {
		case "initialize":
			send(m.ID, map[string]any{"protocolVersion": 2, "capabilities": map[string]any{"session": map[string]any{"prompt": map[string]any{"image": map[string]any{}}}}})
		case "session/new":
			send(m.ID, map[string]string{"sessionId": "session"})
		case "session/resume":
			if mode == "resume-notfound" || mode == "resume-busy" {
				code, message := -32602, "Requested session or tool receipt was not found"
				if mode == "resume-busy" {
					code = -32000
					message = "session busy"
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": code, "message": message}})
			} else {
				send(m.ID, map[string]any{})
			}
		case "session/prompt":
			if mode == "ack-loss" {
				os.Exit(0)
			}
			if mode == "disconnect" {
				send(m.ID, map[string]any{})
				os.Exit(0)
			}
			send(m.ID, map[string]any{})
			update(map[string]any{"sessionUpdate": "state_update", "state": "idle"})
			if mode == "timeout" {
				continue
			}
			update(map[string]any{"sessionUpdate": "state_update", "state": "running"})
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m", "content": map[string]any{"type": "text", "text": "partial"}})
			update(map[string]any{"sessionUpdate": "agent_message", "messageId": "m", "content": []map[string]any{{"type": "text", "text": "complete"}}})
			if mode == "permission" {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "permission", "method": "session/request_permission", "params": map[string]any{"sessionId": "session", "options": []map[string]string{{"kind": "allow_always", "optionId": "always"}, {"kind": "allow_once", "optionId": "once"}}}})
				if !scanner.Scan() {
					os.Exit(3)
				}
				var reply struct {
					Result struct {
						Outcome struct {
							Outcome  string
							OptionID string `json:"optionId"`
						}
					}
				}
				_ = json.Unmarshal(scanner.Bytes(), &reply)
				update(map[string]any{"sessionUpdate": "agent_message", "messageId": "m", "content": []map[string]any{{"type": "text", "text": reply.Result.Outcome.Outcome + ":" + reply.Result.Outcome.OptionID}}})
			}
			update(map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn"})
		case "session/cancel":
			os.Exit(0)
		}
	}
	os.Exit(0)
}
func fakeACP(t *testing.T, mode string) *ACPClient {
	c := testConfig(t)
	c.Deerflow.Args = []string{"-test.run=^TestHelperProcess$", "--"}
	c.Deerflow.Env = map[string]string{"BUZZ_ADAPTER_TEST_HELPER": mode}
	c.Deerflow.Timeout = 2
	c.Deerflow.PromptTimeout = .25
	a := NewACP(c)
	t.Cleanup(a.Close)
	return a
}
func TestACPV2ProjectionAndResume(t *testing.T) {
	for _, mode := range []string{"normal", "resume-notfound", "resume-busy"} {
		t.Run(mode, func(t *testing.T) {
			a := fakeACP(t, mode)
			ctx := context.Background()
			if e := a.Open(ctx); e != nil {
				t.Fatal(e)
			}
			s, e := a.AttachOrCreate(ctx, "existing")
			if mode == "resume-busy" {
				if e == nil {
					t.Fatal("busy session replaced")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if mode == "normal" {
				s = "session"
			}
			body, e := a.Prompt(ctx, s, []map[string]any{{"type": "text", "text": "test"}}, nil)
			if e != nil || body != "complete" {
				t.Fatal(body, e)
			}
		})
	}
}
func TestACPUncertainFailures(t *testing.T) {
	for _, mode := range []string{"ack-loss", "disconnect", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			a := fakeACP(t, mode)
			if e := a.Open(context.Background()); e != nil {
				t.Fatal(e)
			}
			_, e := a.Prompt(context.Background(), "session", nil, nil)
			var failure *PromptError
			if !errors.As(e, &failure) || !failure.Uncertain {
				t.Fatalf("lost execution boundary: %v", e)
			}
		})
	}
}
func TestACPPermissionPolicy(t *testing.T) {
	for _, policy := range []string{"deny", "allow_once"} {
		t.Run(policy, func(t *testing.T) {
			a := fakeACP(t, "permission")
			a.Config.Deerflow.PermissionPolicy = policy
			if e := a.Open(context.Background()); e != nil {
				t.Fatal(e)
			}
			body, e := a.Prompt(context.Background(), "session", nil, nil)
			if policy == "deny" {
				var failure *PromptError
				if !errors.As(e, &failure) || failure.Uncertain || !strings.Contains(failure.Detail, "permission was declined") {
					t.Fatal("permission denial not reported", e)
				}
				return
			}
			if e != nil || body != "selected:once" {
				t.Fatal(body, e)
			}
		})
	}
}
