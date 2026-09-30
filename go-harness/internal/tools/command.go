package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type commandInput struct {
	Executable     string   `json:"executable,omitempty" jsonschema:"description=Exact allowlisted executable path; do not combine with script"`
	Args           []string `json:"args,omitempty"`
	Script         string   `json:"script,omitempty" jsonschema:"description=Shell script only when the host explicitly enables scripts; do not combine with executable"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
}

type powerShellCommandInput struct {
	Script         string `json:"script" jsonschema:"description=Nonempty PowerShell script to execute in the workspace; use PowerShell syntax"`
	TimeoutSeconds int64  `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
}

type commandTool struct {
	info    *schema.ToolInfo
	backend harness.CommandBackend
}

// Eino's tool adapter discards a returned string whenever an error accompanies
// it. Keep the process receipt on the error so harness middleware can recover
// it without treating a failed command as a successful tool execution.
type commandResultError struct {
	cause  error
	result string
}

func (e *commandResultError) Error() string      { return e.cause.Error() }
func (e *commandResultError) Unwrap() error      { return e.cause }
func (e *commandResultError) ToolResult() string { return e.result }

// CommandTool exposes foreground execution only. Permission and the durable
// execution receipt are enforced by the engine before this endpoint is called.
func CommandTool(backend harness.CommandBackend, provider harness.SandboxProvider) (tool.InvokableTool, error) {
	if backend == nil {
		return nil, errors.New("command backend is required")
	}
	desc := fmt.Sprintf("Execute a foreground command using the configured %s provider in this session's workspace. Return bounded stdout/stderr, state and exit code. Inspect the exit code: a returned result does not imply the command succeeded. Scripts require host authorization. This tool cannot start persistent background jobs.", provider)
	info, err := utils.GoStruct2ToolInfo[commandInput]("execute", desc)
	if provider == harness.SandboxPowerShell {
		desc = "Execute a foreground PowerShell script in this session's workspace. Supply a nonempty script using PowerShell syntax, for example & 'curl.exe' '-s' 'https://example.com'. Executable/args requests are not supported by this provider. Return bounded stdout/stderr, state and exit code. This tool cannot start persistent background jobs."
		info, err = utils.GoStruct2ToolInfo[powerShellCommandInput]("execute", desc)
	}
	if err != nil {
		return nil, err
	}
	return &commandTool{info: info, backend: backend}, nil
}
func (t *commandTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *commandTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	var in commandInput
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return "", harness.MarkToolNotExecuted(fmt.Errorf("invalid command input: %w", err))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", harness.MarkToolNotExecuted(errors.New("command input must contain exactly one JSON object"))
	}
	if !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return "", harness.MarkToolNotExecuted(errors.New("command input must be a JSON object"))
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > int64((1<<63-1)/int64(time.Second)) {
		return "", harness.MarkToolNotExecuted(errors.New("invalid command timeout"))
	}
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	result, runErr := executeCommand(ctx, t.backend, harness.CommandRequest{Executable: in.Executable, Args: in.Args, Script: in.Script, Timeout: time.Duration(in.TimeoutSeconds) * time.Second})
	data, marshalErr := json.Marshal(result)
	output := string(data)
	if err := errors.Join(runErr, marshalErr); err != nil {
		return output, &commandResultError{cause: err, result: output}
	}
	return output, nil
}

// All foreground adapters share cancellation, process joining and receipts.
func executeCommand(ctx context.Context, backend harness.CommandBackend, request harness.CommandRequest) (harness.CommandSnapshot, error) {
	started, err := backend.Start(ctx, request)
	if err != nil {
		return harness.CommandSnapshot{}, err
	}
	result, waitErr := backend.Wait(ctx, started.ID)
	if waitErr != nil {
		// Cancellation of Wait only stops waiting. Join cancellation of the owned
		// command before returning and before the runtime releases its run lease.
		var cancelErr error
		result, cancelErr = backend.Cancel(context.WithoutCancel(ctx), started.ID)
		waitErr = errors.Join(waitErr, cancelErr)
	}
	confirmed := result.TerminationConfirmed && result.State != harness.CommandUncertain
	switch result.State {
	case harness.CommandCompleted:
		if result.ExitCode != nil && *result.ExitCode != 0 {
			waitErr = errors.Join(waitErr, fmt.Errorf("command exited with code %d", *result.ExitCode))
		}
	case harness.CommandFailed:
		if result.ExitCode != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("command exited with code %d", *result.ExitCode))
		} else {
			waitErr = errors.Join(waitErr, errors.New("command failed: "+result.Error))
		}
	case harness.CommandTimedOut:
		waitErr = errors.Join(waitErr, errors.New("command exceeded its execution timeout"))
	case harness.CommandCancelled:
		waitErr = errors.Join(waitErr, context.Canceled)
	default:
		confirmed = false
	}
	if !confirmed {
		waitErr = errors.Join(waitErr, harness.ErrCommandUncertain)
	}
	if confirmed {
		waitErr = errors.Join(waitErr, backend.Release(context.WithoutCancel(ctx), started.ID))
	}
	return result, waitErr
}
