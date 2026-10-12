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

func TestUnavailableOpenCLIKeepsContractAndNotExecutedEvidence(t *testing.T) {
	cause := errors.New("configured OpenCLI executable cannot be found")
	unavailable, err := UnavailableOpenCLITool(cause, []string{"twitter"})
	if err != nil {
		t.Fatal(err)
	}
	regular, err := OpenCLITool(&commandBackendFixture{}, opencli.Launcher{Executable: "/fixed"}, []string{"twitter"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, _ := unavailable.Info(ctx)
	want, _ := regular.Info(ctx)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("availability changed the tool schema: %s != %s", gotJSON, wantJSON)
	}
	_, err = unavailable.InvokableRun(ctx, `{"site":"twitter","command":"timeline"}`)
	var notExecuted *harness.ToolNotExecutedError
	if !errors.As(err, &notExecuted) || !errors.Is(err, cause) || !strings.Contains(err.Error(), "OpenCLI is unavailable") {
		t.Fatalf("unavailable command lacks diagnostic or no-execution evidence: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = unavailable.InvokableRun(ctx, `{}`)
	if !errors.Is(err, context.Canceled) || !errors.As(err, &notExecuted) {
		t.Fatalf("unavailability swallowed cancellation: %v", err)
	}
}
