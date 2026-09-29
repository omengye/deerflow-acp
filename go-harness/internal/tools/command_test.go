package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type commandBackendFixture struct {
	starts, releases int
	snapshot         harness.CommandSnapshot
	wait             func(context.Context, string) (harness.CommandSnapshot, error)
	cancel           func(context.Context, string) (harness.CommandSnapshot, error)
	releaseError     error
}

func (b *commandBackendFixture) Start(context.Context, harness.CommandRequest) (harness.CommandSnapshot, error) {
	b.starts++
	return harness.CommandSnapshot{ID: "owned-command"}, nil
}
func (b *commandBackendFixture) Poll(context.Context, string) (harness.CommandSnapshot, error) {
	return b.snapshot, nil
}
func (b *commandBackendFixture) Wait(ctx context.Context, id string) (harness.CommandSnapshot, error) {
	if b.wait != nil {
		return b.wait(ctx, id)
	}
	return b.snapshot, nil
}
func (b *commandBackendFixture) Cancel(ctx context.Context, id string) (harness.CommandSnapshot, error) {
	if b.cancel != nil {
		return b.cancel(ctx, id)
	}
	return b.snapshot, nil
}
func (b *commandBackendFixture) Release(context.Context, string) error {
	b.releases++
	return b.releaseError
}
func (*commandBackendFixture) Close() error { return nil }

func TestCommandTerminalResultsAndUncertainRetention(t *testing.T) {
	for _, test := range []struct {
		name                     string
		state                    harness.CommandState
		exit                     int
		confirmed, fail, release bool
	}{
		{"success", harness.CommandCompleted, 0, true, false, true},
		{"nonzero", harness.CommandFailed, 7, true, true, true},
		{"inconsistent exit", harness.CommandCompleted, 9, true, true, true},
		{"timeout", harness.CommandTimedOut, 0, true, true, true},
		{"cancelled", harness.CommandCancelled, 0, true, true, true},
		{"uncertain", harness.CommandUncertain, 0, false, true, false},
		{"inconsistent uncertain", harness.CommandUncertain, 0, true, true, false},
		{"not joined", harness.CommandRunning, 0, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &commandBackendFixture{snapshot: harness.CommandSnapshot{ID: "owned-command", State: test.state, ExitCode: &test.exit, TerminationConfirmed: test.confirmed, Stdout: harness.CommandOutput{Text: "partial output"}}}
			command, err := CommandTool(backend, harness.SandboxLocal)
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.InvokableRun(context.Background(), `{"executable":"/allowed"}`)
			if (err != nil) != test.fail {
				t.Fatalf("error=%v", err)
			}
			if !test.release && !errors.Is(err, harness.ErrCommandUncertain) {
				t.Fatalf("missing uncertain error: %v", err)
			}
			if err != nil {
				var receipt interface{ ToolResult() string }
				if !errors.As(err, &receipt) || receipt.ToolResult() != output {
					t.Fatal("error did not carry the process receipt through Eino")
				}
			}
			var got harness.CommandSnapshot
			if err := json.Unmarshal([]byte(output), &got); err != nil {
				t.Fatal(err)
			}
			if got.State != test.state || got.ExitCode == nil || *got.ExitCode != test.exit || got.Stdout.Text != "partial output" {
				t.Fatalf("lost command result: %+v", got)
			}
			if (backend.releases == 1) != test.release {
				t.Fatalf("releases=%d", backend.releases)
			}
		})
	}
}

func TestCommandCancellationJoinsBeforeReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting, cancelling, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	backend := &commandBackendFixture{}
	backend.wait = func(ctx context.Context, id string) (harness.CommandSnapshot, error) {
		close(waiting)
		<-ctx.Done()
		return harness.CommandSnapshot{ID: id}, ctx.Err()
	}
	backend.cancel = func(ctx context.Context, id string) (harness.CommandSnapshot, error) {
		if err := ctx.Err(); err != nil {
			t.Error("cleanup inherited cancelled context")
		}
		close(cancelling)
		<-joined
		return harness.CommandSnapshot{ID: id, State: harness.CommandCancelled, TerminationConfirmed: true}, nil
	}
	command, err := CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := command.InvokableRun(ctx, `{"executable":"/allowed"}`); done <- err }()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("command did not wait")
	}
	cancel()
	select {
	case <-cancelling:
	case <-time.After(3 * time.Second):
		t.Fatal("command did not cancel")
	}
	select {
	case <-done:
		t.Fatal("returned before joining owned command")
	default:
	}
	close(joined)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not finish cleanup")
	}
	if backend.releases != 1 {
		t.Fatal("confirmed command was not released")
	}
}

func TestCommandRejectsMalformedInputBeforeStarting(t *testing.T) {
	backend := &commandBackendFixture{}
	command, err := CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`null`, `[]`, `{"unknown":true}`, `{"executable":"/allowed"} {}`, `{"executable":"/allowed"} trailing`, `{"timeout_seconds":-1}`, `{"timeout_seconds":9223372036854775807}`} {
		if _, err := command.InvokableRun(context.Background(), input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := command.InvokableRun(ctx, `{"executable":"/allowed"}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call=%v", err)
	}
	if backend.starts != 0 {
		t.Fatalf("started %d malformed calls", backend.starts)
	}
}

func TestCommandCleanupFailureRemainsVisible(t *testing.T) {
	failure := errors.New("release failed")
	backend := &commandBackendFixture{snapshot: harness.CommandSnapshot{ID: "owned-command", State: harness.CommandCompleted, TerminationConfirmed: true}, releaseError: failure}
	command, err := CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.InvokableRun(context.Background(), `{"executable":"/allowed"}`)
	if !errors.Is(err, failure) || !strings.Contains(output, "owned-command") {
		t.Fatalf("output=%q error=%v", output, err)
	}
}
