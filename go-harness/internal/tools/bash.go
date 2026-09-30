package tools

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type bashInput struct {
	Description string `json:"description,omitempty"`
	Command     string `json:"command"`
	Timeout     int    `json:"timeout,omitempty"`
}

func BashTool(backend harness.CommandBackend, provider harness.SandboxProvider) (tool.InvokableTool, error) {
	return inferTool("bash", "Run a foreground shell command in the workspace using the configured "+string(provider)+" provider. On Windows powershell, use PowerShell syntax. Requires host shell opt-in and session permission. Returns bounded stdout/stderr and exit code; background jobs are not supported.", func(ctx context.Context, in bashInput) (string, error) {
		if in.Command == "" || in.Timeout < 0 || in.Timeout > 120 {
			return "", harness.MarkToolNotExecuted(errors.New("command is required; timeout must be 0..120 seconds"))
		}
		result, err := executeCommand(ctx, backend, harness.CommandRequest{Script: in.Command, Timeout: time.Duration(in.Timeout) * time.Second})
		out, marshalErr := marshalString(result)
		if err = errors.Join(err, marshalErr); err != nil {
			return out, &commandResultError{cause: err, result: out}
		}
		return out, nil
	})
}
