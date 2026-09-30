package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
)

func TestOpenCLIRejectionCarriesNotExecutedEvidence(t *testing.T) {
	backend := &commandBackendFixture{}
	item, err := OpenCLITool(backend, opencli.Launcher{Executable: "/fixed"}, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{strings.Repeat(" ", 65537), `null`, `[]`, `{"site":"web","command":"get","unknown":true}`, `{"site":"web","command":"get"} {}`, `{"site":"other","command":"get"}`, `{"site":"web","command":"bad command"}`, `{"site":"web","command":"get","arguments":["\u0000"]}`}
	for _, args := range [][]string{make([]string, 129), {strings.Repeat("x", (16<<10)+1)}} {
		raw, _ := json.Marshal(opencliInput{Site: "web", Command: "get", Arguments: args})
		inputs = append(inputs, string(raw))
	}
	for _, input := range inputs {
		_, err := item.InvokableRun(context.Background(), input)
		var evidence *harness.ToolNotExecutedError
		if err == nil || !errors.As(err, &evidence) {
			t.Fatalf("rejection lacks execution evidence: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = item.InvokableRun(ctx, `{"site":"web","command":"get"}`)
	var evidence *harness.ToolNotExecutedError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &evidence) {
		t.Fatalf("cancelled preflight: %v", err)
	}
	if backend.starts != 0 {
		t.Fatalf("rejected calls started %d processes", backend.starts)
	}
}

func TestOpenCLIExecutedFailureDoesNotClaimNotExecuted(t *testing.T) {
	exit := 7
	backend := &commandBackendFixture{snapshot: harness.CommandSnapshot{ID: "command", State: harness.CommandFailed, ExitCode: &exit, TerminationConfirmed: true, Stdout: harness.CommandOutput{Text: "external effect may have occurred"}}}
	item, err := OpenCLITool(backend, opencli.Launcher{Executable: "/fixed"}, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := item.InvokableRun(context.Background(), `{"site":"web","command":"get"}`)
	var evidence *harness.ToolNotExecutedError
	var result interface{ ToolResult() string }
	if err == nil || errors.As(err, &evidence) || !errors.As(err, &result) || result.ToolResult() != output || backend.starts != 1 || !strings.Contains(output, "external effect may have occurred") {
		t.Fatalf("executed failure lost evidence: starts=%d output=%q error=%v", backend.starts, output, err)
	}
}
