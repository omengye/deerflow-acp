// deerflow-memory-migrate imports one explicitly mapped Python DeerMem JSON
// document into a scoped Go harness memory store. It never starts a model.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

type disabledEngine struct{}

func (disabledEngine) Run(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
	return harness.RunResult{}, errors.New("migration command cannot execute a model")
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deerflow-memory-migrate:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("deerflow-memory-migrate", flag.ContinueOnError)
	var source, dataDir, workspace, sessionID, scopeName, userID string
	var apply bool
	flags.StringVar(&source, "source", "", "explicit Python DeerMem memory.json path")
	flags.StringVar(&dataDir, "data-dir", "", "Go harness state directory")
	flags.StringVar(&workspace, "workspace", "", "existing Go session workspace")
	flags.StringVar(&sessionID, "session-id", "", "existing Go session ID used for ownership and scope")
	flags.StringVar(&scopeName, "scope", "", "explicit destination: session, workspace, or user")
	flags.StringVar(&userID, "user-id", "", "explicit Go host user identity for user scope")
	flags.BoolVar(&apply, "apply", false, "atomically write accepted facts; otherwise preview only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || source == "" {
		return errors.New("--source is required and positional arguments are not accepted")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
	if err != nil {
		return err
	}
	if !apply {
		report, err := memory.PreviewLegacy(raw)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(report)
	}
	if dataDir == "" || workspace == "" || sessionID == "" || scopeName == "" {
		return errors.New("--apply requires --data-dir, --workspace, --session-id, and --scope")
	}
	kind := harness.MemoryScope(scopeName)
	if kind != harness.MemorySession && kind != harness.MemoryWorkspace && kind != harness.MemoryUser {
		return errors.New("--scope must be session, workspace, or user")
	}
	if kind == harness.MemoryUser && userID == "" || kind != harness.MemoryUser && userID != "" {
		return errors.New("--user-id is required only for user scope")
	}
	ctx := context.Background()
	client, err := deerflow.Open(ctx, deerflow.Config{DataDir: dataDir, MemoryUserID: userID, Engine: disabledEngine{}})
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.LoadSession(ctx, sessionID, workspace, false, nil); err != nil {
		return err
	}
	report, err := client.ImportDeerMem(ctx, sessionID, kind, raw)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}
