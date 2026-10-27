// The offline migrator runs under the same state lock as the native daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"os"
	"path/filepath"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "memory migration failed:", err)
		os.Exit(1)
	}
}
func run() error {
	data := flag.String("data-dir", "", "Native harness data directory")
	plan := flag.String("plan", "", "Reviewed JSON manifest path")
	flag.Parse()
	if *data == "" || *plan == "" {
		return fmt.Errorf("data-dir and plan required")
	}
	lock := flock.New(filepath.Join(*data, "runtime.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return fmt.Errorf("daemon state is busy")
	}
	defer lock.Close()
	raw, err := os.ReadFile(*plan)
	if err != nil {
		return err
	}
	var input struct {
		Name  string                 `json:"name"`
		Items []memory.MigrationItem `json:"items"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	db, err := sqlite.Open(filepath.Join(*data, "harness.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	s, err := memory.New(context.Background(), db.DB())
	if err != nil {
		return err
	}
	report, err := s.MigrateSingleUser(context.Background(), input.Name, input.Items)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
