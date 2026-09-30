package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestInferToolRejectedArgumentsCarryNotExecutedEvidence(t *testing.T) {
	type input struct {
		Count int `json:"count"`
	}
	calls := 0
	item, err := inferTool("fixture_write", "fixture", func(context.Context, input) (string, error) {
		calls++
		return "OK", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `{"count":"wrong"}`, `{"unknown":true}`, `{"count":1} {}`, `{"count":1} trailing`} {
		_, err := item.InvokableRun(context.Background(), raw)
		var evidence *harness.ToolNotExecutedError
		if !errors.As(err, &evidence) {
			t.Fatalf("missing rejection evidence for %q: %v", raw, err)
		}
	}
	if calls != 0 {
		t.Fatalf("malformed input invoked function %d times", calls)
	}
	if _, err := item.InvokableRun(context.Background(), `{"count":1}`); err != nil || calls != 1 {
		t.Fatalf("valid input failed: %d %v", calls, err)
	}
}

func TestNativeReadOnlyFailuresCarryTerminalEvidence(t *testing.T) {
	for _, name := range []string{"ls", "glob", "grep", "web_search", "web_fetch", "image_search", "fixture_write"} {
		t.Run(name, func(t *testing.T) {
			cause := errors.New("fixture read failure")
			item, err := inferTool(name, "fixture", func(context.Context, struct{}) (string, error) { return "", cause })
			if err != nil {
				t.Fatal(err)
			}
			_, err = item.InvokableRun(context.Background(), `{}`)
			var evidence *harness.ToolNoEffectError
			if !errors.Is(err, cause) || errors.As(err, &evidence) != (name != "fixture_write") {
				t.Fatalf("incorrect evidence for %s: %v", name, err)
			}
		})
	}
}
