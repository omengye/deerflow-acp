// deerflow-skill-go authors local skills without changing the live registry.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillauthor"
)

func run(args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: deerflow-skill-go init|validate|package --path PATH [--name NAME --description TEXT | --output FILE.skill]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("path", "", "existing parent directory for init; skill directory for validate/package")
	name := flags.String("name", "", "new skill name")
	description := flags.String("description", "", "trigger description")
	output := flags.String("output", "", "new .skill archive outside the input directory")
	if err := flags.Parse(args[1:]); err != nil {
		return nil, err
	}
	if *path == "" || flags.NArg() != 0 {
		return nil, errors.New("--path is required; unexpected positional arguments")
	}
	ctx := context.Background()
	switch args[0] {
	case "init":
		if *output != "" {
			return nil, errors.New("init does not accept --output")
		}
		created, err := skillauthor.Initialize(ctx, *path, *name, *description)
		return map[string]any{"path": created, "complete": false}, err
	case "validate":
		if *name != "" || *description != "" || *output != "" {
			return nil, errors.New("validate accepts only --path")
		}
		return skillauthor.Validate(ctx, *path)
	case "package":
		if *name != "" || *description != "" || *output == "" {
			return nil, errors.New("package requires --path and --output")
		}
		return skillauthor.Package(ctx, *path, *output)
	default:
		return nil, fmt.Errorf("unknown skill authoring operation %q", args[0])
	}
}

func main() {
	data, err := run(os.Args[1:])
	envelope := map[string]any{"ok": err == nil}
	if err != nil {
		envelope["error"] = err.Error()
	} else {
		envelope["data"] = data
	}
	_ = json.NewEncoder(os.Stdout).Encode(envelope)
	if err != nil {
		os.Exit(1)
	}
}
