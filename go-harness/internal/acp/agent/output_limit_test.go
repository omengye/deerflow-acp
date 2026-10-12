package agent_test

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestOutputLimitNoticeUsesStableProtocolMetadata(t *testing.T) {
	f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "output_limit", Text: "Incomplete after bounded continuation; partial answer preserved."}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{StopReason: "max_tokens"}, nil
	}))
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "long answer"))
	event := update(t, c.read(t), sid, "agent_message_chunk")
	if chunkText(t, event) != "Incomplete after bounded continuation; partial answer preserved." {
		t.Fatal("output-limit notice missing")
	}
	stopReason(t, c.success(t, id), "max_tokens")
}
