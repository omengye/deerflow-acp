package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
)

type opencliInput struct {
	Description string   `json:"description" jsonschema:"description=Brief reason for invoking OpenCLI"`
	Site        string   `json:"site" jsonschema:"description=Host-allowlisted OpenCLI adapter/site name"`
	Command     string   `json:"command" jsonschema:"description=Adapter command name"`
	Arguments   []string `json:"arguments,omitempty" jsonschema:"description=Literal CLI arguments; never provide a shell command string"`
}

func OpenCLITool(backend harness.CommandBackend, plan opencli.Launcher, sites []string) (tool.InvokableTool, error) {
	if backend == nil {
		return nil, errors.New("OpenCLI command backend is required")
	}
	info, err := utils.GoStruct2ToolInfo[opencliInput]("host_opencli", "Run a host-installed OpenCLI adapter command. Allowed sites: "+strings.Join(sites, ", ")+". Browser adapters require the user's OpenCLI browser bridge. Returns bounded stdout/stderr, exit_code, state and truncation flags. JSON output is requested by default. This can have side effects and requires the session's tool approval policy.")
	if err != nil {
		return nil, err
	}
	return &opencliTool{info: info, backend: backend, plan: plan, sites: slices.Clone(sites)}, nil
}

type opencliTool struct {
	info    *schema.ToolInfo
	backend harness.CommandBackend
	plan    opencli.Launcher
	sites   []string
}

func (t *opencliTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *opencliTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	if len(raw) > 64<<10 {
		return "", harness.MarkToolNotExecuted(errors.New("OpenCLI input exceeds 64 KiB"))
	}
	var in opencliInput
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return "", harness.MarkToolNotExecuted(errors.New("invalid OpenCLI input"))
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return "", harness.MarkToolNotExecuted(errors.New("OpenCLI input must be exactly one object"))
	}
	if !slices.Contains(t.sites, in.Site) || !harness.ValidOpenCLIWord(in.Site) {
		return "", harness.MarkToolNotExecuted(errors.New("OpenCLI site is not allowed by host configuration"))
	}
	if !harness.ValidOpenCLIWord(in.Command) || len(in.Arguments) > 128 {
		return "", harness.MarkToolNotExecuted(errors.New("invalid OpenCLI command or argument count"))
	}
	formatted := false
	for _, arg := range in.Arguments {
		if len(arg) > 16<<10 || strings.ContainsRune(arg, 0) {
			return "", harness.MarkToolNotExecuted(errors.New("invalid OpenCLI argument"))
		}
		if arg == "-f" || arg == "--format" || strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "-f=") {
			formatted = true
		}
	}
	argv := append(slices.Clone(t.plan.Prefix), in.Site, in.Command)
	argv = append(argv, in.Arguments...)
	if !formatted {
		argv = append(argv, "--format", "json")
	}
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	result, runErr := executeCommand(ctx, t.backend, harness.CommandRequest{Executable: t.plan.Executable, Args: argv})
	data, marshalErr := json.Marshal(struct {
		ExitCode             *int                 `json:"exit_code"`
		Stdout               string               `json:"stdout"`
		Stderr               string               `json:"stderr"`
		State                harness.CommandState `json:"state"`
		StdoutTruncated      bool                 `json:"stdout_truncated"`
		StderrTruncated      bool                 `json:"stderr_truncated"`
		TerminationConfirmed bool                 `json:"termination_confirmed"`
	}{result.ExitCode, result.Stdout.Text, result.Stderr.Text, result.State, result.Stdout.Truncated, result.Stderr.Truncated, result.TerminationConfirmed})
	output := string(data)
	if err := errors.Join(runErr, marshalErr); err != nil {
		return output, &commandResultError{cause: fmt.Errorf("OpenCLI: %w", err), result: output}
	}
	return output, nil
}
