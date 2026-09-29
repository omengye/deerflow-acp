package agent_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type subagentWireUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string `json:"sessionUpdate"`
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
		Status     string `json:"status"`
		Meta       struct {
			Deerflow struct {
				State string `json:"state"`
			} `json:"deerflow"`
		} `json:"_meta"`
		Content json.RawMessage `json:"content"`
	} `json:"update"`
}

func collectSubagentUpdates(t *testing.T, c *client, responseID int, sessionID string) []subagentWireUpdate {
	t.Helper()
	var updates []subagentWireUpdate
	for {
		msg := c.read(t)
		if msg.Method == "" {
			if string(msg.ID) != strconv.Itoa(responseID) || msg.Error != nil {
				t.Fatalf("unexpected response: %+v", msg)
			}
			return updates
		}
		if msg.Method != "session/update" {
			t.Fatalf("unexpected notification: %+v", msg)
		}
		var event subagentWireUpdate
		if err := json.Unmarshal(msg.Params, &event); err != nil {
			t.Fatal(err)
		}
		if event.SessionID != sessionID {
			t.Fatalf("wrong session update: %+v", event)
		}
		if event.Update.Kind == "tool_call" || event.Update.Kind == "tool_call_update" {
			updates = append(updates, event)
		}
	}
}

func readSubagentUpdate(t *testing.T, c *client, sessionID string) subagentWireUpdate {
	t.Helper()
	msg := c.read(t)
	if msg.Method != "session/update" {
		t.Fatalf("expected subagent update: %+v", msg)
	}
	var event subagentWireUpdate
	if err := json.Unmarshal(msg.Params, &event); err != nil {
		t.Fatal(err)
	}
	if event.SessionID != sessionID || event.Update.Kind != "tool_call" && event.Update.Kind != "tool_call_update" {
		t.Fatalf("unexpected subagent update: %+v", event)
	}
	return event
}

func checkSubagentUpdates(t *testing.T, updates []subagentWireUpdate, kinds, statuses, states []string) string {
	t.Helper()
	if len(updates) != len(kinds) {
		t.Fatalf("subagent updates=%+v, want %d", updates, len(kinds))
	}
	id := updates[0].Update.ToolCallID
	if !strings.HasPrefix(id, "subagent:") || !strings.HasSuffix(id, ":delegate") {
		t.Fatalf("invalid delegation ID: %q", id)
	}
	for i, event := range updates {
		if event.Update.Kind != kinds[i] || event.Update.ToolCallID != id || event.Update.Status != statuses[i] || event.Update.Meta.Deerflow.State != states[i] {
			t.Fatalf("subagent update[%d]=%+v, kind=%q status=%q state=%q", i, event, kinds[i], statuses[i], states[i])
		}
	}
	return id
}

func TestACPSubagentLifecycleSessionIsolationAndReplay(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if req.Input[0].Text == "start" {
			for _, event := range []harness.RunEvent{
				{Kind: "subagent_start", ToolCallID: "delegate", ToolName: "task", Status: "in_progress"},
				{Kind: "subagent_suspended", ToolCallID: "delegate", ToolName: "task", Status: "waiting_input"},
			} {
				if err := emit(ctx, event); err != nil {
					return harness.RunResult{}, err
				}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return harness.RunResult{}, ctx.Err()
			}
		}
		for _, event := range []harness.RunEvent{
			{Kind: "subagent_resumed", ToolCallID: "delegate", ToolName: "task", Status: "in_progress"},
			{Kind: "subagent_end", ToolCallID: "delegate", ToolName: "task", Status: "completed", Content: []harness.Content{{Type: "text", Text: "child done"}}},
		} {
			if err := emit(ctx, event); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	first := c.newSession(t, f.cwd)
	second := c.newSession(t, f.cwd)
	firstPrompt := c.request(t, "session/prompt", promptParams(first, "start"))
	initial := []subagentWireUpdate{readSubagentUpdate(t, c, first), readSubagentUpdate(t, c, first)}
	if initial[0].Update.Title != "task" {
		t.Fatalf("missing delegation title: %+v", initial[0])
	}
	// A different session can reuse the native call ID. Its resumed event needs
	// a fresh ACP card even while the first session's card is suspended.
	independent := collectSubagentUpdates(t, c, c.request(t, "session/prompt", promptParams(second, "resume")), second)
	secondID := checkSubagentUpdates(t, independent, []string{"tool_call", "tool_call_update", "tool_call_update"}, []string{"in_progress", "in_progress", "completed"}, []string{"", "running", ""})
	var result []struct {
		Type    string          `json:"type"`
		Content harness.Content `json:"content"`
	}
	if err := json.Unmarshal(independent[2].Update.Content, &result); err != nil {
		t.Fatal(err)
	}
	if independent[2].Update.Status != "completed" || len(result) != 1 || result[0].Content.Text != "child done" {
		t.Fatalf("missing delegation result: %+v", independent[2])
	}
	releaseOnce.Do(func() { close(release) })
	resumed := collectSubagentUpdates(t, c, firstPrompt, first)
	firstID := checkSubagentUpdates(t, append(initial, resumed...), []string{"tool_call", "tool_call_update", "tool_call_update", "tool_call_update"}, []string{"in_progress", "in_progress", "in_progress", "completed"}, []string{"", "waiting_input", "running", ""})
	if firstID == secondID {
		t.Fatalf("different sessions reused ACP delegation ID %q", firstID)
	}
	// The same persisted events rebuild the card in order after reconnect/load.
	c.success(t, c.request(t, "session/close", map[string]any{"sessionId": first}))
	replayed := collectSubagentUpdates(t, c, c.request(t, "session/load", map[string]any{"sessionId": first, "cwd": f.cwd, "mcpServers": []any{}}), first)
	replayedID := checkSubagentUpdates(t, replayed, []string{"tool_call", "tool_call_update", "tool_call_update", "tool_call_update"}, []string{"in_progress", "in_progress", "in_progress", "completed"}, []string{"", "waiting_input", "running", ""})
	if replayedID != firstID {
		t.Fatalf("replay changed delegation ID %q to %q", firstID, replayedID)
	}
}
