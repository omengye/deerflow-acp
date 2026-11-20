package sqlite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyReadsWithoutChangingTheDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(filepath.Join(dir, "missing.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing database = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("opening a missing database created %d files", len(entries))
	}
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("a directory opened as a database")
	}

	path := filepath.Join(dir, "harness.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`CREATE TABLE probe(value TEXT); INSERT INTO probe VALUES('written')`); err != nil {
		t.Fatal(err)
	}
	// The writer stays open, as a running runtime would.
	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM probe`).Scan(&value); err != nil || value != "written" {
		t.Fatalf("read %q, %v", value, err)
	}
	if _, err := db.Exec(`INSERT INTO probe VALUES('reader')`); err == nil {
		t.Fatal("a read-only connection wrote")
	}
	if _, err := store.DB().Exec(`INSERT INTO probe VALUES('later')`); err != nil {
		t.Fatalf("the writer is blocked by the reader: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM probe`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("reader saw %d rows, %v", count, err)
	}
}
