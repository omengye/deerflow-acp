package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestACPContextUsageUpdateAndHistoryReplay(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "usage", Usage: &harness.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}); err != nil {
			return harness.RunResult{}, err
		}
		if req.Input[0].Text == "with window" {
			if err := emit(ctx, harness.RunEvent{Kind: "context_usage", ContextUsage: &harness.ContextUsage{Size: 4096, Used: 33}}); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	check := func(responseID int, wantUpdate bool) {
		t.Helper()
		seen := 0
		for {
			msg := c.read(t)
			if msg.Method == "" {
				if string(msg.ID) != strconv.Itoa(responseID) || msg.Error != nil || !bytes.Contains(msg.Result, []byte(`"totalTokens":120`)) {
					t.Fatalf("usage response=%+v", msg)
				}
				if (seen == 1) != wantUpdate {
					t.Fatalf("context updates=%d want=%v", seen, wantUpdate)
				}
				return
			}
			var wire struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					Kind string `json:"sessionUpdate"`
					Size int64  `json:"size"`
					Used int64  `json:"used"`
				} `json:"update"`
			}
			if msg.Method != "session/update" || json.Unmarshal(msg.Params, &wire) != nil || wire.SessionID != sid || wire.Update.Kind != "usage_update" || wire.Update.Size != 4096 || wire.Update.Used != 33 {
				t.Fatalf("context update=%+v", msg)
			}
			seen++
		}
	}
	check(c.request(t, "session/prompt", promptParams(sid, "with window")), true)
	check(c.request(t, "session/prompt", promptParams(sid, "without window")), false)
	c.success(t, c.request(t, "session/close", map[string]any{"sessionId": sid}))
	replay := c.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	seen := 0
	for {
		msg := c.read(t)
		if msg.Method == "" {
			if string(msg.ID) != strconv.Itoa(replay) || msg.Error != nil || seen != 1 {
				t.Fatalf("replay response=%+v updates=%d", msg, seen)
			}
			break
		}
		var wire struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind string `json:"sessionUpdate"`
				Size int64  `json:"size"`
				Used int64  `json:"used"`
			} `json:"update"`
		}
		if msg.Method != "session/update" || json.Unmarshal(msg.Params, &wire) != nil || wire.SessionID != sid {
			t.Fatalf("replay update=%+v", msg)
		}
		if wire.Update.Kind == "usage_update" {
			if wire.Update.Size != 4096 || wire.Update.Used != 33 {
				t.Fatalf("replayed context=%+v", wire)
			}
			seen++
		}
	}
}
