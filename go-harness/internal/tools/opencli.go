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
	Site        string   `json:"site,omitempty" jsonschema:"description=Host-allowlisted adapter/site. Omit for top-level list, help, doctor or version"`
	Command     string   `json:"command" jsonschema:"description=Adapter action, help or --help. Without site only list/help/doctor/version and their help/version flags are supported"`
	Arguments   []string `json:"arguments,omitempty" jsonschema:"description=Literal CLI arguments; never provide a shell command string"`
}

func OpenCLITool(backend harness.CommandBackend, plan opencli.Launcher, sites []string) (tool.InvokableTool, error) {
	if backend == nil {
		return nil, errors.New("OpenCLI command backend is required")
	}
	info, err := utils.GoStruct2ToolInfo[opencliInput]("host_opencli", "Run a host-installed OpenCLI command. Allowed adapter sites: "+strings.Join(sites, ", ")+". Discover commands with {command:'list'} or {site:'twitter',command:'help'}; use help with arguments:['search'] for one adapter command's options. Omit site for list/help/doctor/version. Only adapter execution and list request JSON by default; help/version/doctor use their native output. Browser adapters require the user's OpenCLI browser bridge. Returns bounded stdout/stderr, exit_code, state, truncation flags and termination confirmation. Execution can have side effects and requires the session's tool approval policy.")
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
	if len(in.Arguments) > 128 {
		return "", harness.MarkToolNotExecuted(errors.New("invalid OpenCLI command or argument count"))
	}
	for _, arg := range in.Arguments {
		if len(arg) > 16<<10 || strings.ContainsRune(arg, 0) {
			return "", harness.MarkToolNotExecuted(errors.New("invalid OpenCLI argument"))
		}
	}
	command, localOnly, err := opencliCommand(in, t.sites)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	argv := append(slices.Clone(t.plan.Prefix), command...)
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	result, runErr := executeCommand(ctx, t.backend, harness.CommandRequest{Executable: t.plan.Executable, Args: argv})
	if localOnly && result.TerminationConfirmed && !errors.Is(runErr, harness.ErrCommandUncertain) {
		// A deliberately planned help/list/version route cannot dispatch an
		// adapter action. Process failures still retain their diagnostic result.
		runErr = harness.MarkToolNoEffect(runErr)
	}
	data, marshalErr := json.Marshal(struct {
		ExitCode             *int                 `json:"exit_code"`
		State                harness.CommandState `json:"state"`
		TerminationConfirmed bool                 `json:"termination_confirmed"`
		StdoutTruncated      bool                 `json:"stdout_truncated"`
		StderrTruncated      bool                 `json:"stderr_truncated"`
		Stdout               string               `json:"stdout"`
		Stderr               string               `json:"stderr"`
	}{result.ExitCode, result.State, result.TerminationConfirmed, result.Stdout.Truncated, result.Stderr.Truncated, result.Stdout.Text, result.Stderr.Text})
	output := string(data)
	if err := errors.Join(runErr, marshalErr); err != nil {
		return output, &commandResultError{cause: fmt.Errorf("OpenCLI: %w", err), result: output}
	}
	return output, nil
}

// opencliCommand plans known CLI surfaces instead of treating every request as
// an adapter action. It never changes the host's adapter allowlist.
func opencliCommand(in opencliInput, sites []string) ([]string, bool, error) {
	help := in.Command == "help" || in.Command == "--help" || in.Command == "-h"
	if in.Site != "" {
		if !slices.Contains(sites, in.Site) || !harness.ValidOpenCLIWord(in.Site) {
			return nil, false, errors.New("OpenCLI site is not allowed by host configuration")
		}
		if help {
			targets, options, err := opencliHelpArguments(in.Arguments, 1)
			if err != nil {
				return nil, false, err
			}
			args := append([]string{in.Site}, targets...)
			return append(append(args, "--help"), options...), true, nil
		}
		if !harness.ValidOpenCLIWord(in.Command) {
			return nil, false, errors.New("invalid OpenCLI command")
		}
		args := []string{in.Site, in.Command}
		if opencliOnlyHelpArguments(in.Arguments) {
			_, options, _ := opencliHelpArguments(in.Arguments, 0)
			return append(append(args, "--help"), options...), true, nil
		}
		args = append(args, in.Arguments...)
		return opencliJSONArguments(args, 2), false, nil
	}
	if help {
		targets, options, err := opencliHelpArguments(in.Arguments, 2)
		if err != nil {
			return nil, false, err
		}
		if len(targets) > 0 && !slices.Contains(sites, targets[0]) {
			return nil, false, errors.New("OpenCLI help target is not allowed by host configuration")
		}
		return append(append(targets, "--help"), options...), true, nil
	}
	switch in.Command {
	case "version", "--version", "-V":
		if len(in.Arguments) != 0 {
			return nil, false, errors.New("OpenCLI version takes no arguments")
		}
		return []string{"--version"}, true, nil
	case "list":
		if _, _, err := opencliHelpArguments(in.Arguments, 0); err != nil {
			return nil, false, errors.New("OpenCLI list only supports output format and help flags")
		}
		if opencliOnlyHelpArguments(in.Arguments) {
			_, options, _ := opencliHelpArguments(in.Arguments, 0)
			return append([]string{"list", "--help"}, options...), true, nil
		}
		return opencliJSONArguments(append([]string{"list"}, in.Arguments...), 1), true, nil
	case "doctor":
		if opencliOnlyHelpArguments(in.Arguments) {
			_, options, _ := opencliHelpArguments(in.Arguments, 0)
			return append([]string{"doctor", "--help"}, options...), true, nil
		}
		for _, arg := range in.Arguments {
			if arg != "-v" && arg != "--verbose" {
				return nil, false, errors.New("OpenCLI doctor only supports verbose and help flags")
			}
		}
		// Doctor performs a live browser probe and can start/restart the
		// browser daemon; it does not have a purely local no-effect contract.
		return append([]string{"doctor"}, in.Arguments...), false, nil
	default:
		return nil, false, errors.New("without site, OpenCLI command must be list, help, doctor or version")
	}
}

func opencliOnlyHelpArguments(args []string) bool {
	help := false
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		help = help || arg == "--help" || arg == "-h"
	}
	_, _, err := opencliHelpArguments(args, 0)
	return help && err == nil
}

func opencliHelpArguments(args []string, maxTargets int) (targets, options []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			continue
		}
		format := ""
		if arg == "-f" || arg == "--format" {
			if i+1 >= len(args) {
				return nil, nil, errors.New("OpenCLI output format needs a value")
			}
			i++
			format = args[i]
		} else if strings.HasPrefix(arg, "--format=") {
			format = strings.TrimPrefix(arg, "--format=")
		} else if strings.HasPrefix(arg, "-f=") {
			format = strings.TrimPrefix(arg, "-f=")
		} else if strings.HasPrefix(arg, "-f") && len(arg) > 2 {
			format = arg[2:]
		} else {
			if !harness.ValidOpenCLIWord(arg) || len(targets) >= maxTargets {
				return nil, nil, errors.New("invalid OpenCLI help target or option")
			}
			targets = append(targets, arg)
			continue
		}
		if !slices.Contains([]string{"table", "yaml", "yml", "json", "plain", "md", "csv"}, format) {
			return nil, nil, errors.New("invalid OpenCLI output format")
		}
		options = append(options, "--format", format)
	}
	return targets, options, nil
}

func opencliJSONArguments(args []string, start int) []string {
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// Format options must precede the end-of-options separator so
			// literal arguments remain literal rather than swallowing our flag.
			result := append(slices.Clone(args[:i]), "--format", "json")
			return append(result, args[i:]...)
		}
		if arg == "--help" || arg == "-h" || arg == "-f" || arg == "--format" || strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "-f=") || strings.HasPrefix(arg, "-f") && len(arg) > 2 {
			return args
		}
	}
	return append(args, "--format", "json")
}
