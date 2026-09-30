package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func TestOpenCLIHelpAndDiscoveryPlan(t *testing.T) {
	for _, test := range []struct {
		name  string
		input opencliInput
		argv  []string
	}{
		{"legacy_site_help", opencliInput{Site: "twitter", Command: "help"}, []string{"twitter", "--help"}},
		{"site_help_flag", opencliInput{Site: "twitter", Command: "--help"}, []string{"twitter", "--help"}},
		{"site_short_help", opencliInput{Site: "twitter", Command: "-h"}, []string{"twitter", "--help"}},
		{"command_help", opencliInput{Site: "twitter", Command: "help", Arguments: []string{"search"}}, []string{"twitter", "search", "--help"}},
		{"command_help_argument", opencliInput{Site: "twitter", Command: "search", Arguments: []string{"--help"}}, []string{"twitter", "search", "--help"}},
		{"explicit_help_format", opencliInput{Site: "twitter", Command: "help", Arguments: []string{"search", "-f", "json"}}, []string{"twitter", "search", "--help", "--format", "json"}},
		{"top_help", opencliInput{Command: "help"}, []string{"--help"}},
		{"top_help_flag", opencliInput{Command: "--help"}, []string{"--help"}},
		{"top_target_help", opencliInput{Command: "help", Arguments: []string{"twitter", "search"}}, []string{"twitter", "search", "--help"}},
		{"list", opencliInput{Command: "list"}, []string{"list", "--format", "json"}},
		{"list_format", opencliInput{Command: "list", Arguments: []string{"-fyaml"}}, []string{"list", "-fyaml"}},
		{"list_help", opencliInput{Command: "list", Arguments: []string{"--help"}}, []string{"list", "--help"}},
		{"doctor", opencliInput{Command: "doctor"}, []string{"doctor"}},
		{"doctor_verbose", opencliInput{Command: "doctor", Arguments: []string{"--verbose"}}, []string{"doctor", "--verbose"}},
		{"doctor_help", opencliInput{Command: "doctor", Arguments: []string{"--help"}}, []string{"doctor", "--help"}},
		{"version", opencliInput{Command: "version"}, []string{"--version"}},
		{"version_flag", opencliInput{Command: "--version"}, []string{"--version"}},
		{"version_short", opencliInput{Command: "-V"}, []string{"--version"}},
		{"adapter", opencliInput{Site: "twitter", Command: "search", Arguments: []string{"literal & $(NO_EXEC) %PATH%"}}, []string{"twitter", "search", "literal & $(NO_EXEC) %PATH%", "--format", "json"}},
		{"literal_help", opencliInput{Site: "twitter", Command: "tweet", Arguments: []string{"--", "--help"}}, []string{"twitter", "tweet", "--format", "json", "--", "--help"}},
		{"literal_format", opencliInput{Site: "twitter", Command: "tweet", Arguments: []string{"--", "--format=json"}}, []string{"twitter", "tweet", "--format", "json", "--", "--format=json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			exit := 0
			backend := &captureOpenCLIBackend{commandBackendFixture: commandBackendFixture{snapshot: harness.CommandSnapshot{State: harness.CommandCompleted, ExitCode: &exit, TerminationConfirmed: true}}}
			item, err := OpenCLITool(backend, opencli.Launcher{Executable: "/fixed", Prefix: []string{"fixed.js"}}, []string{"web", "twitter"})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(test.input)
			if _, err := item.InvokableRun(context.Background(), string(encoded)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(backend.request.Args, append([]string{"fixed.js"}, test.argv...)) || backend.request.Script != "" || backend.request.Executable != "/fixed" || backend.starts != 1 {
				t.Fatalf("unsafe or incorrect command: %+v", backend.request)
			}
		})
	}
}

func TestOpenCLIDiscoveryDoesNotExpandExecutionPolicy(t *testing.T) {
	backend := &commandBackendFixture{}
	item, err := OpenCLITool(backend, opencli.Launcher{Executable: "/fixed"}, []string{"web", "twitter"})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"site":"browser","command":"state"}`,
		`{"site":"github","command":"--help"}`,
		`{"command":"help","arguments":["browser"]}`,
		`{"command":"help","arguments":["twitter","search","extra"]}`,
		`{"command":"help","arguments":["--","twitter"]}`,
		`{"site":"twitter","command":"help","arguments":["tweet","message"]}`,
		`{"command":"daemon","arguments":["stop"]}`,
		`{"command":"external","arguments":["register","curl"]}`,
		`{"command":"list","arguments":["twitter"]}`,
		`{"command":"list","arguments":["--format"]}`,
		`{"command":"list","arguments":["--format=unknown"]}`,
		`{"command":"doctor","arguments":["--format","json"]}`,
		`{"command":"version","arguments":["twitter"]}`,
	} {
		_, err := item.InvokableRun(context.Background(), input)
		var rejected *harness.ToolNotExecutedError
		if !errors.As(err, &rejected) {
			t.Fatalf("unsafe diagnostic escaped preflight: %s %v", input, err)
		}
	}
	if backend.starts != 0 {
		t.Fatal("rejected diagnostic started a process")
	}
}

func TestOpenCLITerminalDiagnosticFailureEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		input     opencliInput
		confirmed bool
		noEffect  bool
	}{
		{"site_help", opencliInput{Site: "twitter", Command: "help"}, true, true},
		{"command_help", opencliInput{Site: "twitter", Command: "search", Arguments: []string{"--help"}}, true, true},
		{"list", opencliInput{Command: "list"}, true, true},
		{"version", opencliInput{Command: "version"}, true, true},
		{"unconfirmed_help", opencliInput{Site: "twitter", Command: "help"}, false, false},
		{"doctor_live_probe", opencliInput{Command: "doctor"}, true, false},
		{"adapter", opencliInput{Site: "twitter", Command: "tweet", Arguments: []string{"message"}}, true, false},
		{"literal_help_after_separator", opencliInput{Site: "twitter", Command: "tweet", Arguments: []string{"--", "--help"}}, true, false},
		{"help_as_option_value", opencliInput{Site: "twitter", Command: "search", Arguments: []string{"--query", "--help"}}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			exit := 1
			backend := &commandBackendFixture{snapshot: harness.CommandSnapshot{State: harness.CommandFailed, ExitCode: &exit, TerminationConfirmed: test.confirmed, Stderr: harness.CommandOutput{Text: "fixture diagnostic"}}}
			item, err := OpenCLITool(backend, opencli.Launcher{Executable: "/fixed"}, []string{"twitter"})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(test.input)
			output, err := item.InvokableRun(context.Background(), string(encoded))
			var noEffect *harness.ToolNoEffectError
			var notExecuted *harness.ToolNotExecutedError
			var result interface{ ToolResult() string }
			if err == nil || errors.As(err, &noEffect) != test.noEffect || errors.As(err, &notExecuted) || !errors.As(err, &result) || result.ToolResult() != output {
				t.Fatalf("incorrect failure evidence: %s %v", output, err)
			}
		})
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
