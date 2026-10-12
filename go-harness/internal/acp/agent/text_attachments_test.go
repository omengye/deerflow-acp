package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestTextAttachmentACPWireSnapshotsAndRejectsForgedResources(t *testing.T) {
	var f *fixture
	var calls atomic.Int32
	f = newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		if len(req.Input) != 1 || req.Input[0].Asset == nil || req.Input[0].Text != "" {
			t.Error("embedded text was not snapshotted")
			return harness.RunResult{}, harness.ErrInvalidInput
		}
		store := f.service.Assets
		ref := req.Input[0].Asset
		chunk, err := store.ReadAttachment(ctx, req.Session, ref.ID, 0, 4096)
		if err != nil || chunk.Text != "key,value\nhello,42\n" || ref.Name != "notes #1.csv" {
			t.Errorf("text read: %+v, %v, %+v", chunk, err, ref)
		}
		return harness.RunResult{StopReason: "end_turn"}, err
	}))
	enableMedia(t, f)
	c := connect(t, f.service)
	id := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var capabilities map[string]any
	if err := json.Unmarshal(c.success(t, id), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustJSON(t, capabilities)), `"embeddedContext":true`) {
		t.Fatal("text capability missing")
	}
	sid := c.newSession(t, f.cwd)
	resource := func(uri string) map[string]any {
		return map[string]any{"type": "resource", "resource": map[string]any{"uri": uri, "mimeType": "text/plain", "text": "key,value\nhello,42\n"}}
	}
	id = c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{resource("attachment:///notes%20%231.csv")}})
	_, response := collectMediaResponse(t, c, id)
	if response.Error != nil || calls.Load() != 1 {
		t.Fatal(response.Error, calls.Load())
	}
	for _, uri := range []string{"file:///outside.txt", "attachment://other/file.txt", "attachment:///../file.txt", "attachment:///file.pdf", "attachment:///file.txt?q=x"} {
		id = c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{resource(uri)}})
		_, response = collectMediaResponse(t, c, id)
		if response.Error == nil || calls.Load() != 1 {
			t.Fatalf("invalid resource accepted: %s", uri)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
