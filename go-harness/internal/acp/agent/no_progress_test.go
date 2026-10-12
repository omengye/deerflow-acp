package agent_test

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestNoProgressIsVisibleAndNotSuccessfulCompletion(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "no_progress", Text: "Task incomplete: repeated identical observations."}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{Limit: "no_progress"}, harness.ErrNoProgress
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "work"))
	event := update(t, c.read(t), sid, "agent_message_chunk")
	if chunkText(t, event) != "Task incomplete: repeated identical observations." {
		t.Fatal(event)
	}
	response := c.response(t, id)
	if response.Error == nil || response.Error.Code != -32023 {
		t.Fatalf("expected no-progress failure: %+v", response)
	}
}
