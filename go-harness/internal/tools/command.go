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
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

// commandInput is the decoded request. The schema types below advertise only
// the fields a tool instance supports.
type commandInput struct {
	Credential     *secrets.CredentialBinding `json:"credential,omitempty" jsonschema_description:"Service login scoped to this tool and origin. Supply service origin and username_env and/or key_env; never include actual values."`
	Executable     string                     `json:"executable,omitempty"`
	Args           []string                   `json:"args,omitempty"`
	Script         string                     `json:"script,omitempty"`
	TimeoutSeconds int64                      `json:"timeout_seconds,omitempty"`
	Secrets        map[string]string          `json:"secrets,omitempty"`
	SecretOut      bool                       `json:"secret_out,omitempty"`
}

type commandSchema struct {
	Executable     string   `json:"executable,omitempty" jsonschema:"description=Exact allowlisted executable path; do not combine with script"`
	Args           []string `json:"args,omitempty"`
	Script         string   `json:"script,omitempty" jsonschema:"description=Shell script only when the host explicitly enables scripts; do not combine with executable"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
}

type commandSecretSchema struct {
	Credential     *secrets.CredentialBinding `json:"credential,omitempty" jsonschema_description:"Service login scoped to this tool and origin. Supply service origin and username_env and/or key_env; never include actual values."`
	Executable     string                     `json:"executable,omitempty" jsonschema:"description=Exact allowlisted executable path; do not combine with script"`
	Args           []string                   `json:"args,omitempty"`
	Script         string                     `json:"script,omitempty" jsonschema:"description=Shell script only when the host explicitly enables scripts; do not combine with executable"`
	TimeoutSeconds int64                      `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
	Secrets        map[string]string          `json:"secrets,omitempty" jsonschema_description:"Optional map from environment variable to vault secret name, for example {GITHUB_TOKEN: GITHUB_TOKEN}. A string secret becomes the variable value; a file secret becomes the path of a private file. Reference secrets only through these variables and never paste values into commands. Known values in output appear as [secret:NAME]."`
	SecretOut      bool                       `json:"secret_out,omitempty" jsonschema_description:"Set DEERFLOW_SECRET_OUT to a private directory. After the command ends, each file written there and named like a secret (for example $DEERFLOW_SECRET_OUT/API_TOKEN) is stored as that secret's new version, so refreshed tokens or generated keys are saved without printing them."`
}

type powerShellCommandInput struct {
	Script         string `json:"script" jsonschema:"description=Nonempty PowerShell script to execute in the workspace; use PowerShell syntax"`
	TimeoutSeconds int64  `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
}

type powerShellSecretInput struct {
	Credential     *secrets.CredentialBinding `json:"credential,omitempty" jsonschema_description:"Service login scoped to this tool and origin. Supply service origin and username_env and/or key_env; never include actual values."`
	Script         string                     `json:"script" jsonschema:"description=Nonempty PowerShell script to execute in the workspace; use PowerShell syntax"`
	TimeoutSeconds int64                      `json:"timeout_seconds,omitempty" jsonschema:"description=Optional shorter execution timeout; zero uses host policy"`
	Secrets        map[string]string          `json:"secrets,omitempty" jsonschema_description:"Optional map from environment variable to vault secret name, for example {GITHUB_TOKEN: GITHUB_TOKEN}. A string secret becomes the variable value; a file secret becomes the path of a private file. Reference secrets only through these variables and never paste values into commands. Known values in output appear as [secret:NAME]. In PowerShell read them as $env:NAME."`
	SecretOut      bool                       `json:"secret_out,omitempty" jsonschema_description:"Set DEERFLOW_SECRET_OUT to a private directory. After the command ends, each file written there and named like a secret (for example $env:DEERFLOW_SECRET_OUT/API_TOKEN) is stored as that secret's new version, so refreshed tokens or generated keys are saved without printing them."`
}

type commandTool struct {
	toolidentity.Native
	info    *schema.ToolInfo
	backend harness.CommandBackend
	vault   *secrets.Vault
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
func CommandTool(backend harness.CommandBackend, provider harness.SandboxProvider, options ...CommandOption) (tool.InvokableTool, error) {
	if backend == nil {
		return nil, errors.New("command backend is required")
	}
	o := applyCommandOptions(options)
	desc := fmt.Sprintf("Execute a foreground command using the configured %s provider in this session's workspace. Return bounded stdout/stderr, state and exit code. Inspect the exit code: a returned result does not imply the command succeeded. Scripts require host authorization. This tool cannot start persistent background jobs.", provider)
	powerShell := provider == harness.SandboxPowerShell
	if powerShell {
		desc = "Execute a foreground PowerShell script in this session's workspace. Supply a nonempty script using PowerShell syntax, for example & 'curl.exe' '-s' 'https://example.com'. Executable/args requests are not supported by this provider. Return bounded stdout/stderr, state and exit code. This tool cannot start persistent background jobs."
	}
	var info *schema.ToolInfo
	var err error
	switch {
	case powerShell && o.vault != nil:
		info, err = utils.GoStruct2ToolInfo[powerShellSecretInput]("execute", desc)
	case powerShell:
		info, err = utils.GoStruct2ToolInfo[powerShellCommandInput]("execute", desc)
	case o.vault != nil:
		info, err = utils.GoStruct2ToolInfo[commandSecretSchema]("execute", desc)
	default:
		info, err = utils.GoStruct2ToolInfo[commandSchema]("execute", desc)
	}
	if err != nil {
		return nil, err
	}
	return &commandTool{Native: toolidentity.Builtin("execute"), info: info, backend: backend, vault: o.vault}, nil
}
func (t *commandTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

// checkedInput validates the same request before permission and at dispatch. Errors
// returned to the model must never echo argument values or unknown field names.
func (t *commandTool) checkedInput(args string) (commandInput, error) {
	var in commandInput
	var bash bashSecretInput
	var plainBash bashInput
	var target any = &in
	if t.info.Name == "bash" {
		target = &bash
		if t.vault == nil {
			target = &plainBash
		}
	}
	reject := func(message string) (commandInput, error) {
		return commandInput{}, &harness.ToolArgumentError{Code: "invalid_command_arguments", Message: message}
	}
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.DisallowUnknownFields()
	if !strings.HasPrefix(strings.TrimSpace(args), "{") {
		return reject("command input must be a JSON object")
	}
	if err := decoder.Decode(target); err != nil {
		if t.info.Name == "execute" && err.Error() == `json: unknown field "description"` {
			return reject("execute does not accept the description field")
		}
		return reject("invalid command input; use the fields and types advertised by the command tool")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return reject("command input must contain exactly one JSON object")
	}
	if t.info.Name == "bash" {
		if t.vault == nil {
			bash.Command, bash.Timeout = plainBash.Command, plainBash.Timeout
		}
		if bash.Command == "" || bash.Timeout < 0 || bash.Timeout > 120 {
			return reject("bash requires a command and a timeout of 0..120 seconds")
		}
		in = commandInput{Script: bash.Command, TimeoutSeconds: int64(bash.Timeout), Secrets: bash.Secrets, SecretOut: bash.SecretOut, Credential: bash.Credential}
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > int64((1<<63-1)/int64(time.Second)) {
		return reject("invalid command timeout")
	}
	if t.vault == nil && (len(in.Secrets) > 0 || in.SecretOut || in.Credential != nil) {
		return reject("secrets are not available for this command tool")
	}
	if in.Credential != nil {
		if _, _, err := in.Credential.Bind("tool:"+t.info.Name, in.Secrets); err != nil {
			return reject("invalid credential binding; supply an HTTP(S) or SSH service origin, a valid account label, and distinct permitted environment variable names")
		}
	} else if err := secrets.ValidateBindings(in.Secrets); err != nil {
		return reject("invalid secrets mapping; supply permitted environment variable names and valid vault secret names")
	}
	if validator, ok := t.backend.(interface {
		ValidateCommand(harness.CommandRequest) error
	}); ok {
		if err := validator.ValidateCommand(in.request()); err != nil {
			return commandInput{}, err
		}
	}
	return in, nil
}

func (in commandInput) request() harness.CommandRequest {
	return harness.CommandRequest{Executable: in.Executable, Args: in.Args, Script: in.Script, Timeout: time.Duration(in.TimeoutSeconds) * time.Second}
}

func (t *commandTool) PrepareToolArguments(_ context.Context, args string) (string, error) {
	if _, err := t.checkedInput(args); err != nil {
		var invalid *harness.ToolArgumentError
		if errors.As(err, &invalid) {
			return "", err
		}
		// Host launch-policy diagnostics describe constraints without including
		// argument values. Direct callers retain the backend's typed error.
		return "", &harness.ToolArgumentError{Code: "invalid_command_request", Message: err.Error()}
	}
	return args, nil
}

func (t *commandTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	in, err := t.checkedInput(args)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	result, report, runErr := ExecuteWithSecrets(ctx, t.backend, in.request(), SecretRequest{Credential: in.Credential, Vault: t.vault, Bindings: in.Secrets, WriteBack: in.SecretOut, Actor: t.info.Name})
	data, marshalErr := json.Marshal(commandOutput{CommandSnapshot: result, Secrets: report})
	output := string(data)
	if err := errors.Join(runErr, marshalErr); err != nil {
		return output, &commandResultError{cause: err, result: output}
	}
	return output, nil
}

// All foreground adapters share cancellation, process joining and receipts.
func executeCommand(ctx context.Context, backend harness.CommandBackend, request harness.CommandRequest) (harness.CommandSnapshot, error) {
	return ExecuteCommand(ctx, backend, request)
}

// ExecuteCommand shares foreground process ownership with skill execution.
func ExecuteCommand(ctx context.Context, backend harness.CommandBackend, request harness.CommandRequest) (harness.CommandSnapshot, error) {
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
	preExecutionFailure := waitErr == nil && result.NotExecuted && result.State == harness.CommandFailed
	terminalFailure := waitErr == nil && confirmed && ctx.Err() == nil &&
		((result.State == harness.CommandFailed || result.State == harness.CommandCompleted) && result.ExitCode != nil && *result.ExitCode != 0 || result.State == harness.CommandTimedOut)
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
		releaseErr := backend.Release(context.WithoutCancel(ctx), started.ID)
		waitErr = errors.Join(waitErr, releaseErr)
		if preExecutionFailure && releaseErr == nil && ctx.Err() == nil {
			waitErr = harness.MarkToolNotExecuted(waitErr)
		} else if terminalFailure && releaseErr == nil && ctx.Err() == nil {
			waitErr = harness.MarkToolExecutionFailed(waitErr)
		}
	}
	return result, waitErr
}
