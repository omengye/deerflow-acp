package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/statebackup"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: deerflow-state-go backup|verify|restore [flags]")
	}
	flags := flag.NewFlagSet("deerflow-state-go", flag.ContinueOnError)
	data := flags.String("data-dir", "", "offline runtime data directory, or new restore destination")
	output := flags.String("output", "", "new backup directory; parent must exist")
	backup := flags.String("backup", "", "snapshot directory to verify or restore")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fail("invalid flags")
	}
	if flags.NArg() != 0 {
		fail("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var m statebackup.Manifest
	var err error
	switch os.Args[1] {
	case "backup":
		if *data == "" || *output == "" || *backup != "" {
			fail("backup requires --data-dir and --output")
		}
		m, err = statebackup.Backup(ctx, *data, *output)
	case "verify":
		if *backup == "" || *data != "" || *output != "" {
			fail("verify requires --backup")
		}
		m, err = statebackup.Verify(ctx, *backup)
	case "restore":
		if *backup == "" || *data == "" || *output != "" {
			fail("restore requires --backup and a new --data-dir")
		}
		m, err = statebackup.Restore(ctx, *backup, *data)
	default:
		fail("unknown operation")
	}
	if err != nil {
		fail(err.Error())
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "operation": os.Args[1], "version": m.Version, "files": len(m.Files), "created_at": m.CreatedAt})
}

func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
