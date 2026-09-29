package client

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCleanupOrphansKeepsLiveSessions(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE harness_sessions (id TEXT PRIMARY KEY); INSERT INTO harness_sessions(id) VALUES ('live')`); err != nil {
		t.Fatal(err)
	}
	hash := func(id string) string { value := sha256.Sum256([]byte(id)); return fmt.Sprintf("%x", value[:]) }
	for _, category := range []string{"acp-workspaces", "acp-agent-sessions"} {
		for _, id := range []string{"live", "deleted"} {
			path := filepath.Join(root, category, hash(id))
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "state"), []byte("saved"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := CleanupOrphans(context.Background(), root, db); err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"acp-workspaces", "acp-agent-sessions"} {
		if _, err := os.Stat(filepath.Join(root, category, hash("live"))); err != nil {
			t.Fatalf("live %s removed: %v", category, err)
		}
		if _, err := os.Stat(filepath.Join(root, category, hash("deleted"))); !os.IsNotExist(err) {
			t.Fatalf("deleted %s survived: %v", category, err)
		}
	}
}
