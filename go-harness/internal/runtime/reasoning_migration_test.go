package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSessionReasoningLegacyMigration(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	_, err = native.DB().ExecContext(ctx, `
CREATE TABLE harness_sessions(id TEXT PRIMARY KEY,cwd TEXT NOT NULL,title TEXT NOT NULL,mode TEXT NOT NULL,model TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE harness_session_configs(session_id TEXT PRIMARY KEY REFERENCES harness_sessions(id),approval_mode TEXT NOT NULL DEFAULT 'ask',subagents INTEGER NOT NULL DEFAULT 1,version INTEGER NOT NULL DEFAULT 1);
INSERT INTO harness_sessions VALUES('legacy','workspace','Old title','default','model','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z');
INSERT INTO harness_session_configs VALUES('legacy','read_only',0,7);`)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := store.Session(ctx, "legacy")
	if err != nil || x.Title != "Old title" || x.ApprovalMode != "read_only" || x.Subagents || x.ConfigVersion != 7 || x.ReasoningEffort != "" {
		t.Fatalf("migration changed existing config: %+v %v", x, err)
	}
	before := executionConfigDigest(x)
	x.ReasoningEffort = "high"
	if before == executionConfigDigest(x) {
		t.Fatal("execution identity ignores effort")
	}
	if err := store.SaveConfig(ctx, x); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(ctx, native.DB()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.List(ctx, "", "", 10)
	if err != nil || len(loaded) != 1 || loaded[0].ReasoningEffort != "high" {
		t.Fatalf("list: %+v %v", loaded, err)
	}
	managed, err := store.ListForManagement(ctx, 10)
	if err != nil || len(managed) != 1 || managed[0].ReasoningEffort != "high" {
		t.Fatalf("management list: %+v %v", managed, err)
	}
}
