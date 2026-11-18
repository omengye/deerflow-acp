package deerflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func stage5SDKHistory(t *testing.T, db *sql.DB) [32]byte {
	t.Helper()
	var all []string
	// Explicit old columns exclude the additive provenance field. This covers
	// IDs, revisions, raw receipts, request SHA and the installed profile ID.
	for _, query := range []string{
		"SELECT id,scope_key,head_revision,deleted,created_at FROM memory_facts",
		"SELECT fact_id,revision,scope_revision,content,category,confidence,source_id,source_kind,source_actor_id,source_run_id,source_input_id,source_event_sequence,policy_version,deleted,created_at FROM memory_fact_revisions",
		"SELECT operation_id,scope_key,request_sha,receipt,created_at FROM memory_operations",
		"SELECT scope_key,slot_key,fact_id FROM memory_slots",
		"SELECT * FROM memory_profile",
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values, ptrs := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, query+"\n"+string(encoded))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	sort.Strings(all)
	encoded, _ := json.Marshal(all)
	return sha256.Sum256(encoded)
}

func TestMemoryStage5InstalledCopySDKOpenClose(t *testing.T) {
	path := os.Getenv("MEMORY_STAGE5_DATABASE_COPY")
	if path == "" {
		t.Skip("requires an offline consistent database copy; supplied path is read only")
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "harness.db"), backup, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	before := stage5SDKHistory(t, db.DB())
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	enabled := false
	cfg := Config{DataDir: dir, MemorySingleUser: true, MemoryEnabled: &enabled, DisableSubagents: true,
		Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
			calls.Add(1)
			return harness.RunResult{}, errors.New("offline rehearsal cannot execute a model")
		})}
	for range 2 {
		c, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		after := stage5SDKHistory(t, c.store.DB())
		var version int
		err = c.store.DB().QueryRow("SELECT max(version) FROM memory_schema_migrations").Scan(&version)
		closeErr := c.Close()
		if err != nil || closeErr != nil || version != 5 || after != before || calls.Load() != 0 {
			t.Fatalf("SDK migration: version=%d history_equal=%v calls=%d query=%v close=%v", version, after == before, calls.Load(), err, closeErr)
		}
	}
	t.Logf("SDK repeated open/close preserved history_sha256=%x; model_calls=0", before)
}
