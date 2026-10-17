package agent_test

import (
	"context"
	"encoding/base64"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDocumentAttachmentACPBinaryWire(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(_ context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		if len(req.Input) != 1 || req.Input[0].Asset == nil || req.Input[0].Data != "" || req.Input[0].Asset.Name != "report.pdf" {
			t.Error("binary not snapshotted")
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	enableMedia(t, f)
	c := connect(t, f.service)
	mediaInitialize(t, c)
	sid := c.newSession(t, f.cwd)
	resource := map[string]any{"uri": "attachment:///report.pdf", "mimeType": "application/pdf", "blob": base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 fixture"))}
	id := c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "resource", "resource": resource}}})
	_, response := collectMediaResponse(t, c, id)
	if response.Error != nil || calls.Load() != 1 {
		t.Fatal(response.Error, calls.Load())
	}
	resource["text"] = "ambiguous"
	id = c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "resource", "resource": resource}}})
	_, response = collectMediaResponse(t, c, id)
	if response.Error == nil || calls.Load() != 1 {
		t.Fatal("mixed text/blob accepted")
	}
}
