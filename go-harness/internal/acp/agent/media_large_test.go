//go:build !race

package agent_test

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Exercise the actual 64 MiB JSON-RPC frame boundary with two 20 MiB images.
// Race runs cover the same transport/storage path with small payloads.
func TestTwoTwentyMiBImagesThroughRealPipeAndLoad(t *testing.T) {
	f := newFixture(t, engineFunc(func(_ context.Context, r harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if len(r.Input) != 2 || r.Input[0].Asset == nil || r.Input[1].Asset == nil || r.Input[0].Data != "" || r.Input[1].Data != "" {
			t.Error("large input was not normalized")
		}
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	enableMedia(t, f, "fake-model")
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	image := mediaImage(int(harness.MaxInputImageBytes))
	id := c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []harness.Content{image, image}})
	_, response := collectMediaResponse(t, c, id)
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	id = c.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	updates, response := collectMediaResponse(t, c, id)
	if response.Error != nil || len(updates) != 2 {
		t.Fatalf("large replay updates=%d err=%v", len(updates), response.Error)
	}
	for _, update := range updates {
		if len(update.Update.Content) < len(image.Data) {
			t.Fatal("large image replay truncated")
		}
	}
}
