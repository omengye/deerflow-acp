// deerflow-config-go owns the portable desktop configuration without Python.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/internal/desktopconfig"
)

func main() {
	var options desktopconfig.Options
	flags := flag.NewFlagSet("deerflow-config-go", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Config, "config", "", "configuration YAML")
	flags.StringVar(&options.UserData, "user-data", "", "portable user data")
	flags.StringVar(&options.Resources, "resources", "", "bundled resources")
	var data any
	var err error
	if err = flags.Parse(os.Args[1:]); err == nil {
		if options.Config == "" || options.UserData == "" || options.Resources == "" || flags.NArg() != 1 {
			err = fmt.Errorf("usage: deerflow-config-go --config PATH --user-data PATH --resources PATH OPERATION")
		} else {
			var input json.RawMessage
			if raw, readErr := io.ReadAll(io.LimitReader(os.Stdin, 4<<20+1)); readErr != nil {
				err = fmt.Errorf("read request: %w", readErr)
			} else if len(raw) > 4<<20 {
				err = fmt.Errorf("request exceeds 4 MiB")
			} else {
				input = raw
				data, err = desktopconfig.Run(options, flags.Arg(0), input)
			}
		}
	}
	output := map[string]any{"ok": err == nil}
	if err != nil {
		output["error"] = err.Error()
	} else {
		output["data"] = data
	}
	_ = json.NewEncoder(os.Stdout).Encode(output)
	if err != nil {
		os.Exit(1)
	}
}
